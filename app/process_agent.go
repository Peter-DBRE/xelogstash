package app

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/billgraziano/mssqlodbc"
	"github.com/billgraziano/xelogstash/config"
	"github.com/billgraziano/xelogstash/logstash"
	"github.com/billgraziano/xelogstash/sink"
	"github.com/billgraziano/xelogstash/status"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

type jobResult struct {
	Name           string `json:"name"`
	InstanceID     int    `json:"instance_id"`
	JobID          string `json:"job_id"`
	StepID         int    `json:"step_id"`
	StepName       string `json:"step_name"`
	JobName        string `json:"job_name"`
	Message        string `json:"message"`
	RunStatus      int    `json:"run_status"`
	RunStatusText  string
	RunDuration    int       `json:"run_duration"`
	TimestampLocal time.Time `json:"timestamp"`
	TimestampUTC   time.Time
	FQDN           string `json:"mssql_fqdn"`
	Computer       string `json:"mssql_computer"`
	Server         string `json:"mssql_server_name"`
	Version        string `json:"mssql_version"`
	Domain         string `json:"mssql_domain"`
}

func processAgentJobs(wid int, source config.Source, sinks []sink.Sinker) (result Result, err error) {

	result.Session = "agent_jobs"
	result.Instance = source.FQDN // this will be reset later
	result.Source = source
	dummyFileName := "_dummy_"

	cxn := mssqlodbc.Connection{
		Server:  source.FQDN,
		AppName: "xelogstash.exe",
		Trusted: true,
	}

	connectionString, err := cxn.ConnectionString()
	if err != nil {
		return result, errors.Wrap(err, "mssqlodbc.connectionstring")
	}
	db, err := sql.Open("odbc", connectionString)
	if err != nil {
		return result, errors.Wrap(err, "db.open")
	}
	defer safeClose(db, &err)

	err = db.Ping()
	if err != nil {
		return result, errors.Wrap(err, "db.ping")
	}
	defer safeClose(db, &err)

	info, err := GetInstance(db, source.FQDN)
	if err != nil {
		return result, errors.Wrap(err, "getinstance")
	}
	result.Instance = info.Server

	err = status.SwitchV2(wid, source.Prefix, info.Domain, info.Server, status.ClassAgentJobs, result.Session)
	if err != nil {
		return result, errors.Wrap(err, "status.switchv2")
	}

	// do the dupe check based on the actual instance since that's what is stored
	err = status.CheckDupe(info.Domain, result.Instance, status.ClassAgentJobs, result.Session)
	if err != nil {
		return result, errors.Wrap(err, "dupe.check")
	}

	//appStart := time.Now()

	sf, err := status.NewFile(info.Domain, result.Instance, status.ClassAgentJobs, result.Session)
	if err != nil {
		return result, errors.Wrap(err, "status.newfile")
	}
	_, lastInstanceID, _, err := sf.GetOffset()
	if err != nil {
		return result, errors.Wrap(err, "status.getoffset")
	}

	// var ls *logstash.Logstash
	// if appConfig.Logstash != "" {
	// 	ls, err = logstash.NewHost(appConfig.Logstash, 180)
	// 	if err != nil {
	// 		return result, errors.Wrap(err, "logstash-new")
	// 	}
	// 	// TODO ignore the error for now
	// 	defer ls.Close()
	// }

	// Setup the Elastic client
	// var elasticBuffer bytes.Buffer
	// var esclient *elasticsearch.Client
	// if len(globalConfig.Elastic.Addresses) > 0 && globalConfig.Elastic.Username != "" && globalConfig.Elastic.Password != "" {
	// 	esclient, err = eshelper.NewClient(globalConfig.Elastic.Addresses, globalConfig.Elastic.ProxyServer, globalConfig.Elastic.Username, globalConfig.Elastic.Password)
	// 	if err != nil {
	// 		return result, errors.Wrap(err, "eshelper.NewClient")
	// 	}
	// }

	// sinks, err := globalConfig.GetSinks()
	// if err != nil {
	// 	return result, errors.Wrap(err, "globalconfig.getsinks")
	// }
	for i := range sinks {
		id := strings.Replace(info.Server, "\\", "_", -1)
		err = sinks[i].Open(id)
		if err != nil {
			return result, errors.Wrapf(err, "sink: %s", id)
		}
		defer sinks[i].Close()
	}

	// TODO if source.Rows is very small, it will never get
	// as far as the lookback date
	// If we don't have a previous value (lastInstanceID),
	//   select them all, and break when we have enough
	var query string

	cte := `
		; WITH CTE AS (
			SELECT 
				CASE 
					WHEN H.step_id = 0 THEN 'agent_job' 
					ELSE 'agent_job_step'
				END AS [name]
				,H.instance_id
				,H.job_id
				,H.step_id
				,H.step_name
				,J.[name] AS [job_name]
				,H.[message] as [msg]
				,H.[run_status]
				,H.[run_duration]
				,convert( datetime,
					SUBSTRING(CAST(run_date AS VARCHAR(8)),1,4) + '-' + 
					SUBSTRING(CAST(run_date AS VARCHAR(8)),5,2) + '-'+ 
					SUBSTRING(CAST(run_date AS VARCHAR(8)),7,2) + ' ' +
					convert(varchar, run_time/10000)+':'+
					convert(varchar, run_time%10000/100)+':'+
					convert(varchar, run_time%100)+'.000' ) AS [timestamp_local]
				FROM	[msdb].[dbo].[sysjobhistory] H WITH(NOLOCK)
				JOIN	[msdb].[dbo].[sysjobs] J WITH(NOLOCK) on J.[job_id] = H.[job_id]
				WHERE	1=1
				-- AND  H.[run_status] NOT IN (2, 4) -- Don't want retry or in progress
				--AND		H.[instance_id] > ?
				--ORDER BY H.[instance_id] ASC
		)
		`

	query = cte + `
		
		SELECT *
			, CONVERT(VARCHAR(30), CAST(DATEADD(mi, -1 * DATEDIFF(MINUTE, GETUTCDATE(), GETDATE()), timestamp_local) AS DATETIMEOFFSET), 127) AS timestamp_utc
		FROM CTE
		WHERE [instance_id] > ?
		ORDER BY [instance_id]
		
		`

	rows, err := db.Query(query, lastInstanceID)
	if err != nil {
		return result, errors.Wrap(err, "db.open")
	}

	var instanceID int
	var gotRows bool
	var startAtHit bool

	for rows.Next() {

		// do we have enough rows
		if source.Rows > 0 && result.Rows >= source.Rows {
			break
		}

		readCount.Add(1)

		var tsutc string
		var j jobResult
		err = rows.Scan(&j.Name, &j.InstanceID, &j.JobID, &j.StepID, &j.StepName, &j.JobName, &j.Message, &j.RunStatus, &j.RunDuration, &j.TimestampLocal, &tsutc)
		if err != nil {
			return result, errors.Wrap(err, "rows.scan")
		}

		j.TimestampUTC, err = time.Parse(time.RFC3339Nano, tsutc)
		if err != nil {
			return result, errors.Wrap(err, "invalid utc from sql")
		}

		if j.TimestampUTC.Before(source.StartAt) {
			if !startAtHit {
				log.Debug(fmt.Sprintf("[%d] Source: %s (%s);  'Start At' skipped at least one event", wid, info.Server, "agent-jobs"))
				startAtHit = true
			}
			continue
		}
		if j.TimestampUTC.After(source.StopAt) {
			log.Debug(fmt.Sprintf("[%d] Source: %s (%s);  'Stop At' stopped processing", wid, info.Server, "agent-jobs"))
			break
		}

		instanceID = j.InstanceID

		// TODO do the copies, adds, renames

		// // write to log stash
		// b, err := json.Marshal(j)
		// if err != nil {
		// 	return result, errors.Wrap(err, "json.marshal")
		// }
		// recordString := string(b)

		base := logstash.NewRecord()
		base.Set("name", j.Name)
		base.Set("instance_id", j.InstanceID)
		base.Set("job_id", j.JobID)
		base.Set("step_id", j.StepID)
		base.Set("step_name", j.StepName)
		base.Set("job_name", j.JobName)
		base.Set("message", j.Message)
		base.Set("run_status", j.RunStatus)
		switch j.RunStatus {
		case 0:
			base.Set("run_status_text", "failed")
			base.Set("xe_severity_value", logstash.Error)
			base.Set("xe_severity_keyword", logstash.Error.String())
		case 1:
			base.Set("run_status_text", "succeeded")
			base.Set("xe_severity_value", logstash.Info)
			base.Set("xe_severity_keyword", logstash.Info.String())
		case 2:
			base.Set("run_status_text", "retry")
			base.Set("xe_severity_value", logstash.Warning)
			base.Set("xe_severity_keyword", logstash.Warning.String())
		case 3:
			base.Set("run_status_text", "cancelled")
			base.Set("xe_severity_value", logstash.Warning)
			base.Set("xe_severity_keyword", logstash.Warning.String())
		case 4:
			base.Set("run_status_text", "inprogress")
			base.Set("xe_severity_value", logstash.Info)
			base.Set("xe_severity_keyword", logstash.Info.String())
		default:
			base.Set("run_status_text", "undefined")
			base.Set("xe_severity_value", logstash.Warning)
			base.Set("xe_severity_keyword", logstash.Warning.String())
		}
		base.Set("run_duration", j.RunDuration)
		base.Set("timestamp", j.TimestampUTC)
		base.Set("timestamp_local", j.TimestampLocal)
		base.Set("timestamp_utc_calculated", j.TimestampUTC)

		base.Set("mssql_domain", info.Domain)
		base.Set("mssql_computer", info.Computer)
		base.Set("mssql_server_name", info.Server)
		base.Set("mssql_version", info.Version)
		base.Set("mssql_product_version", info.ProductVersion)

		base.SetIfEmpty("server_instance_name", info.Server)

		// set the description
		var description string
		switch j.Name {
		case "agent_job":
			description = fmt.Sprintf("%s: %s", j.JobName, j.Message)
		case "agent_job_step":
			description = fmt.Sprintf("%s: [%d] %s: %s", j.JobName, j.StepID, j.StepName, j.Message)
		}
		if len(description) > 0 {
			base.Set("xe_description", description)
		}

		// only save if we are doing all or failed and it isn't successful
		if source.AgentJobs == config.JobsAll ||
			(source.AgentJobs == config.JobsFailed && (j.RunStatus == 0 || j.RunStatus == 2 || j.RunStatus == 3)) {
			lr := logstash.NewRecord()
			// if payload field is empty
			if source.PayloadField == "" {
				for k, v := range base {
					lr[k] = v
				}
			} else {
				//fmt.Println(source.PayloadField)
				lr[source.PayloadField] = base
				lr[source.TimestampField] = base["timestamp"]
			}
			// if we're in the root
			if source.TimestampField != "timestamp" && source.PayloadField == "" {
				lr[source.TimestampField] = base["timestamp"]
				delete(lr, "timestamp")
			}

			var rs string
			rs, err = lr.ToJSON()
			if err != nil {
				return result, errors.Wrap(err, "record.tojson")
			}

			// process the adds and such
			rs, err = logstash.ProcessMods(rs, source.Adds, source.Copies, source.Moves)
			if err != nil {
				return result, errors.Wrap(err, "logstash.processmods")
			}

			// TODO if test, write {}
			// if ls != nil {
			// 	err = ls.Writeln(rs)
			// 	if err != nil {
			// 		log.Error("")
			// 		log.Error(fmt.Sprintf("%s\r\n", rs))
			// 		log.Error("")
			// 		return result, errors.Wrap(err, "logstash-writeln")
			// 	}
			// }

			// // Write one entry to the buffer
			// if esclient != nil {
			// 	var esIndex string
			// 	var ok bool
			// 	esIndex, ok = globalConfig.Elastic.EventIndexMap[j.Name]
			// 	if !ok {
			// 		esIndex = globalConfig.Elastic.DefaultIndex
			// 	}
			// 	meta := []byte(fmt.Sprintf(`{ "index" : { "_index" : "%s" } }%s`, esIndex, "\n"))
			// 	espayload := []byte(rs + "\n")
			// 	elasticBuffer.Grow(len(meta) + len(espayload))
			// 	elasticBuffer.Write(meta)
			// 	elasticBuffer.Write(espayload)
			// }

			// err = eshelper.WriteElasticBuffer(esclient, &elasticBuffer)
			// if err != nil {
			// 	return result, errors.Wrap(err, "writeelasticbuffer")
			// }

			// Process all the destinations
			for i := range sinks {
				_, err = sinks[i].Write(j.Name, rs)
				if err != nil {
					newError := errors.Wrap(err, fmt.Sprintf("sink.write: %s", sinks[i].Name()))
					log.Error(newError)
					return result, newError
				}
			}

			// if appConfig.Summary {
			// 	summary.Add(j.Name, &rs)
			// }
			result.Rows++
			totalCount.Add(1)
			eventCount.Add(j.Name, 1)
			serverKey := fmt.Sprintf("%s-%s-%s", info.Domain, strings.Replace(info.Server, "\\", "-", -1), "agent_jobs")
			serverCount.Add(serverKey, 1)
		}

		// write the status field
		err = sf.Save(dummyFileName, int64(j.InstanceID), status.StateSuccess)
		if err != nil {
			return result, errors.Wrap(err, "status.Save")
		}

		gotRows = true
	}

	if gotRows {
		// Process all the destinations
		var lastError error
		for i := range sinks {
			err = sinks[i].Flush()
			if err != nil {
				lastError = errors.Wrapf(err, "sink.flush: %s", sinks[i].Name())
				log.Error(lastError)
			}
			err = sinks[i].Clean()
			if err != nil {
				lastError = errors.Wrapf(err, "sink.clean: %s", sinks[i].Name())
				log.Error(lastError)
			}
		}
		if lastError != nil {
			return result, lastError
		}

		err = sf.Done(dummyFileName, int64(instanceID), status.StateSuccess)
		if err != nil {
			return result, errors.Wrap(err, "status.done")
		}
	}

	return result, nil
}

func jobToRecord(j jobResult) (r logstash.Record, err error) {
	//r.Event = j
	//r.Timestamp = j.Timestamp

	return r, err
}

func parseAgentTime(d, t int) (time.Time, error) {

	dt := strconv.Itoa(d)
	tm := "000000" + strconv.Itoa(t)
	tm = tm[len(tm)-6:]
	// fmt.Println(dt, tm, dt+tm)
	v, err := time.Parse("20060102150405", dt+tm)

	if err != nil {
		return v, err
	}
	return v, nil
}