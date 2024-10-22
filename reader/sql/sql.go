package sql

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "github.com/denisenkom/go-mssqldb" //mssql 驱动
	_ "github.com/go-sql-driver/mysql"   //mysql 驱动
	_ "github.com/lib/pq"                //postgres 驱动
	"github.com/robfig/cron"

	log "k8s.io/klog/v2"

	"github.com/longxiucai/logkit/conf"
	"github.com/longxiucai/logkit/reader"
	. "github.com/longxiucai/logkit/utils/models"
)

const (
	sqlOffsetConnector   = "##"
	sqlSpliter           = ";"
	DefaultMySQLTable    = "SELECT TABLE_NAME FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_TYPE='BASE TABLE' AND TABLE_SCHEMA='DATABASE_NAME';"
	DefaultMySQLDatabase = "SHOW DATABASES;"
	DefaultPGSQLTable    = "SELECT TABLENAME FROM PG_TABLES WHERE SCHEMANAME='SCHEMA_NAME';"
	DefaultMSSQLTable    = "SELECT TABLE_NAME FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_TYPE='BASE TABLE' AND TABLE_CATALOG='DATABASE_NAME' AND TABLE_SCHEMA='SCHEMA_NAME';"

	SupportReminder = "history all magic only support @(YYYY) @(YY) @(MM) @(DD) @(hh) @(mm) @(ss)"
	Wildcards       = "*"

	DefaultDoneRecordsFile = "sql.records"
)

const (
	YEAR = iota
	MONTH
	DAY
	HOUR
	MINUTE
	SECOND
)

const (
	// 获取符合条件的table
	TABLE = iota
	// 获取符合条件的database
	DATABASE
	// 获取数据库表的总条数
	COUNT
)

const (
	// 获取数据条数的函数
	COUNTFUNC = iota
	// 获取读取数据的函数
	READFUNC
)

var (
	_ reader.DaemonReader = &Reader{}
	_ reader.StatsReader  = &Reader{}
	_ reader.DataReader   = &Reader{}
	_ reader.Reader       = &Reader{}
)

var MysqlSystemDB = []string{"information_schema", "performance_schema", "mysql", "sys"}

func init() {
	reader.RegisterConstructor(reader.ModeMySQL, NewReader)
	reader.RegisterConstructor(reader.ModeMSSQL, NewReader)
	reader.RegisterConstructor(reader.ModePostgreSQL, NewReader)
}

type readInfo struct {
	data  Data
	bytes int64
}

type Reader struct {
	meta *reader.Meta
	// Note: 原子操作，用于表示 reader 整体的运行状态
	status int32
	/*
		Note: 原子操作，用于表示获取数据的线程运行状态

		- StatusInit: 当前没有任务在执行
		- StatusRunning: 当前有任务正在执行
		- StatusStopping: 数据管道已经由上层关闭，执行中的任务完成时直接退出无需再处理
	*/
	routineStatus int32

	stopChan chan struct{}
	readChan chan readInfo
	errChan  chan error

	stats     StatsInfo
	statsLock sync.RWMutex

	dbtype      string //数据库类型
	datasource  string //数据源
	database    string //数据库名称
	rawDatabase string // 记录原始数据库
	rawSQLs     string // 原始sql执行列表
	historyAll  bool   // 是否导入历史数据
	rawTable    string // 记录原始数据库表名
	table       string // 数据库表名

	isLoop       bool
	loopDuration time.Duration
	cronSchedule bool //是否为定时任务
	execOnStart  bool
	Cron         *cron.Cron //定时任务
	readBatch    int        // 每次读取的数据量
	offsetKey    string

	encoder           string  // 解码方式
	offsets           []int64 // 当前处理文件的sql的offset
	muxOffsets        sync.RWMutex
	syncSQLs          []string      // 当前在查询的sqls
	syncRecords       SyncDBRecords // 将要append的记录
	doneRecords       SyncDBRecords // 已经读过的记录
	lastDatabase      string        // 读过的最后一条记录的数据库
	lastTabel         string        // 读过的最后一条记录的数据表
	omitDoneDBRecords bool
	schemas           map[string]string
	dbSchema          string
	magicLagDur       time.Duration
	count             int64
	CurrentCount      int64
	countLock         sync.RWMutex
}

func NewReader(meta *reader.Meta, conf conf.MapConf) (reader.Reader, error) {
	var readBatch int
	var dbtype, dataSource, rawDatabase, rawSQLs, cronSchedule, offsetKey, encoder, table, dbSchema string
	var execOnStart, historyAll bool
	dbtype, _ = conf.GetStringOr(reader.KeyMode, reader.ModeMySQL)
	logpath, _ := conf.GetStringOr(reader.KeyLogPath, "")

	var err error
	switch dbtype {
	case reader.ModeMySQL:
		readBatch, _ = conf.GetIntOr(reader.KeyMysqlReadBatch, 100)
		offsetKey, _ = conf.GetStringOr(reader.KeyMysqlOffsetKey, "")
		dataSource, err = conf.GetString(reader.KeyMysqlDataSource)
		if err != nil {
			dataSource = logpath
			if logpath == "" {
				return nil, err
			}
			err = nil
		}
		rawDatabase, _ = conf.GetStringOr(reader.KeyMysqlDataBase, "")
		rawSQLs, _ = conf.GetStringOr(reader.KeyMysqlSQL, "")
		cronSchedule, _ = conf.GetStringOr(reader.KeyMysqlCron, "")
		execOnStart, _ = conf.GetBoolOr(reader.KeyMysqlExecOnStart, true)
		encoder, _ = conf.GetStringOr(reader.KeyMysqlEncoding, "utf8")
		if strings.Contains(encoder, "-") {
			encoder = strings.Replace(strings.ToLower(encoder), "-", "", -1)
		}
		historyAll, _ = conf.GetBoolOr(reader.KeyMysqlHistoryAll, false)
		table, _ = conf.GetStringOr(reader.KyeMysqlTable, "")
	case reader.ModeMSSQL:
		readBatch, _ = conf.GetIntOr(reader.KeyMssqlReadBatch, 100)
		offsetKey, _ = conf.GetStringOr(reader.KeyMssqlOffsetKey, "")
		dataSource, err = conf.GetString(reader.KeyMssqlDataSource)
		if err != nil {
			dataSource = logpath
			if logpath == "" {
				return nil, err
			}
			err = nil
		}
		rawDatabase, err = conf.GetString(reader.KeyMssqlDataBase)
		if err != nil {
			return nil, err
		}
		dbSchema, _ = conf.GetStringOr(reader.KeyMssqlSchema, "dbo")
		rawSQLs, _ = conf.GetStringOr(reader.KeyMssqlSQL, "")
		cronSchedule, _ = conf.GetStringOr(reader.KeyMssqlCron, "")
		execOnStart, _ = conf.GetBoolOr(reader.KeyMssqlExecOnStart, true)
	case reader.ModePostgreSQL:
		readBatch, _ = conf.GetIntOr(reader.KeyPGsqlReadBatch, 100)
		offsetKey, _ = conf.GetStringOr(reader.KeyPGsqlOffsetKey, "")
		dataSource, err = conf.GetString(reader.KeyPGsqlDataSource)
		if err != nil {
			dataSource = logpath
			if logpath == "" {
				return nil, err
			}
			err = nil
		}
		sps := strings.Split(dataSource, " ")
		for _, v := range sps {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			x1s := strings.Split(v, "=")
			if len(x1s) != 2 {
				err = fmt.Errorf("datasource %v is invalid, don't contain space beside symbol '='", dataSource)
				return nil, err
			}
		}
		rawDatabase, err = conf.GetString(reader.KeyPGsqlDataBase)
		if err != nil {
			got := false
			for _, v := range sps {
				if strings.Contains(v, "dbname") {
					rawDatabase = strings.TrimPrefix(v, "dbname=")
					got = true
					break
				}
			}
			if !got {
				return nil, err
			}
		}
		dbSchema, _ = conf.GetStringOr(reader.KeyPGsqlSchema, "public")
		rawSQLs, _ = conf.GetStringOr(reader.KeyPGsqlSQL, "")
		cronSchedule, _ = conf.GetStringOr(reader.KeyPGsqlCron, "")
		execOnStart, _ = conf.GetBoolOr(reader.KeyPGsqlExecOnStart, true)
	default:
		err = fmt.Errorf("%v mode not support in sql reader", dbtype)
		return nil, err
	}
	rawSchemas, _ := conf.GetStringListOr(reader.KeySQLSchema, []string{})
	magicLagDur, _ := conf.GetStringOr(reader.KeyMagicLagDuration, "")
	var mgld time.Duration
	if magicLagDur != "" {
		mgld, err = time.ParseDuration(magicLagDur)
		if err != nil {
			return nil, err
		}
	}
	schemas, err := schemaCheck(rawSchemas)
	if err != nil {
		return nil, err
	}

	var sqls []string
	omitMeta := true
	var offsets []int64
	if rawSQLs != "" {
		offsets, sqls, omitMeta = restoreMeta(meta, rawSQLs, mgld)
	}

	r := &Reader{
		meta:          meta,
		status:        reader.StatusInit,
		routineStatus: reader.StatusInit,
		stopChan:      make(chan struct{}),
		readChan:      make(chan readInfo),
		errChan:       make(chan error),
		datasource:    dataSource,
		database:      rawDatabase,
		rawDatabase:   rawDatabase,
		rawSQLs:       rawSQLs,
		Cron:          cron.New(),
		readBatch:     readBatch,
		offsetKey:     offsetKey,
		syncSQLs:      sqls,
		dbtype:        dbtype,
		execOnStart:   execOnStart,
		historyAll:    historyAll,
		rawTable:      table,
		table:         table,
		magicLagDur:   mgld,
		schemas:       schemas,
		encoder:       encoder,
		dbSchema:      dbSchema,
	}

	if r.rawDatabase == "" {
		r.rawDatabase = "*"
	}
	if r.rawTable == "" {
		r.rawTable = "*"
	}

	if r.rawSQLs == "" {
		valid := checkMagic(r.database) && checkMagic(r.table)
		if !valid {
			err = fmt.Errorf(SupportReminder)
			return nil, err
		}

		r.lastDatabase, r.lastTabel, r.omitDoneDBRecords = r.doneRecords.restoreRecordsFile(r.meta)
	}

	// 如果meta初始信息损坏
	if !omitMeta {
		r.offsets = offsets
	} else {
		r.offsets = make([]int64, len(r.syncSQLs))
	}

	// 定时任务配置串
	if len(cronSchedule) > 0 {
		cronSchedule = strings.ToLower(cronSchedule)
		if strings.HasPrefix(cronSchedule, reader.Loop) {
			r.isLoop = true
			r.loopDuration, err = reader.ParseLoopDuration(cronSchedule)
			if err != nil {
				log.Errorf("Runner[%v] %v %v", r.meta.RunnerName, r.Name(), err)
			}
			if r.loopDuration.Nanoseconds() <= 0 {
				r.loopDuration = 1 * time.Second
			}
		} else {
			r.cronSchedule = true
			err = r.Cron.AddFunc(cronSchedule, r.run)
			if err != nil {
				return nil, err
			}
			log.Infof("Runner[%v] %v Cron job added with schedule <%v>", r.meta.RunnerName, r.Name(), cronSchedule)
		}
	}
	return r, nil
}

func (r *Reader) isStopping() bool {
	return atomic.LoadInt32(&r.status) == reader.StatusStopping
}

func (r *Reader) hasStopped() bool {
	return atomic.LoadInt32(&r.status) == reader.StatusStopped
}

func (r *Reader) Name() string {
	return strings.ToUpper(r.dbtype) + "_Reader:" + r.rawDatabase + "_" + Hash(r.rawSQLs)
}

func (r *Reader) SetMode(mode string, v interface{}) error {
	return errors.New("SQL reader does not support read mode")
}

func (r *Reader) setStatsError(err string) {
	r.statsLock.Lock()
	defer r.statsLock.Unlock()
	r.stats.LastError = err
}

func (r *Reader) sendError(err error) {
	if err == nil {
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			log.Errorf("Reader %q was panicked and recovered from %v", r.Name(), rec)
		}
	}()
	r.errChan <- err
}

func (r *Reader) Start() error {
	if r.isStopping() || r.hasStopped() {
		return errors.New("reader is stopping or has stopped")
	} else if !atomic.CompareAndSwapInt32(&r.status, reader.StatusInit, reader.StatusRunning) {
		log.Warningf("Runner[%v] %q daemon has already started and is running", r.meta.RunnerName, r.Name())
		return nil
	}

	if r.isLoop {
		go func() {
			ticker := time.NewTicker(r.loopDuration)
			defer ticker.Stop()
			for {
				r.run()

				select {
				case <-r.stopChan:
					atomic.StoreInt32(&r.status, reader.StatusStopped)
					log.Infof("Runner[%v] %q daemon has stopped from running", r.meta.RunnerName, r.Name())
					return
				case <-ticker.C:
				}
			}
		}()

	} else {
		if r.execOnStart {
			go r.run()
		}
		r.Cron.Start()
	}
	log.Infof("Runner[%v] %q daemon has started", r.meta.RunnerName, r.Name())
	return nil
}

func (r *Reader) Source() string {
	// 不能把 DataSource 弄出去，包含密码
	return r.dbtype + "_" + r.database
}

func (r *Reader) ReadLine() (string, error) {
	return "", errors.New("method ReadLine is not supported, please use ReadData")
}

func (r *Reader) ReadData() (Data, int64, error) {
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case info := <-r.readChan:
		return info.data, info.bytes, nil
	case err := <-r.errChan:
		return nil, 0, err
	case <-timer.C:
	}

	return nil, 0, nil
}

func (r *Reader) Status() StatsInfo {
	r.statsLock.RLock()
	defer r.statsLock.RUnlock()
	return r.stats
}

// SyncMeta 从队列取数据时同步队列，作用在于保证数据不重复
func (r *Reader) SyncMeta() {
	if r.rawSQLs == "" {
		now := time.Now().String()
		var all string
		dbRecords := r.syncRecords.GetDBRecords()

		for database, tablesRecord := range dbRecords {
			for table, tableInfo := range tablesRecord.GetTable() {
				all += database + sqlOffsetConnector + table + "," +
					strconv.FormatInt(tableInfo.size, 10) + "," +
					strconv.FormatInt(tableInfo.offset, 10) + "," +
					now + "@" + "\n"
			}
		}

		if len(all) <= 0 {
			r.syncRecords.Reset()
			return
		}

		if err := WriteRecordsFile(r.meta.DoneFilePath, all); err != nil {
			log.Errorf("Runner[%v] %v SyncMeta error %v", r.meta.RunnerName, r.Name(), err)
		}
		r.syncRecords.Reset()
		return
	}

	encodeSQLs := make([]string, 0)
	for _, sql := range r.syncSQLs {
		encodeSQLs = append(encodeSQLs, strings.Replace(sql, " ", "@", -1))
	}
	r.muxOffsets.RLock()
	defer r.muxOffsets.RUnlock()
	for _, offset := range r.offsets {
		encodeSQLs = append(encodeSQLs, strconv.FormatInt(offset, 10))
	}
	all := strings.Join(encodeSQLs, sqlOffsetConnector)
	if err := r.meta.WriteOffset(all, int64(len(r.syncSQLs))); err != nil {
		log.Errorf("Runner[%v] %v SyncMeta error %v", r.meta.RunnerName, r.Name(), err)
	}
	return
}

func (r *Reader) Close() error {
	if !atomic.CompareAndSwapInt32(&r.status, reader.StatusRunning, reader.StatusStopping) {
		log.Warningf("Runner[%v] reader %q is not running, close operation ignored", r.meta.RunnerName, r.Name())
		return nil
	}
	log.Infof("Runner[%v] %q daemon is stopping", r.meta.RunnerName, r.Name())
	close(r.stopChan)

	r.Cron.Stop()

	// 如果此时没有 routine 正在运行，则在此处关闭数据管道，否则由 routine 在退出时负责关闭
	if atomic.CompareAndSwapInt32(&r.routineStatus, reader.StatusInit, reader.StatusStopping) {
		close(r.readChan)
		close(r.errChan)
	}
	return nil
}

func schemaCheck(rawSchemas []string) (schemas map[string]string, err error) {
	schemas = make(map[string]string)
	for _, raw := range rawSchemas {
		rs := strings.Fields(raw)
		if len(rs) != 2 {
			err = fmt.Errorf("SQL schema %v not split by space, split lens is %v", raw, len(rs))
			return
		}
		key, vtype := rs[0], rs[1]
		vtype = strings.ToLower(vtype)
		switch vtype {
		case "string", "s":
			vtype = "string"
		case "float", "f":
			vtype = "float"
		case "long", "l":
			vtype = "long"
		default:
			err = fmt.Errorf("schema type %v not supported", vtype)
			return
		}
		schemas[key] = vtype
	}
	return
}

func restoreMeta(meta *reader.Meta, rawSqls string, magicLagDur time.Duration) (offsets []int64, sqls []string, omitMeta bool) {
	now := time.Now().Add(-magicLagDur)
	sqls = updateSqls(rawSqls, now)
	omitMeta = true
	sqlAndOffsets, length, err := meta.ReadOffset()
	if err != nil {
		log.Errorf("Runner[%v] %v -meta data is corrupted err:%v, omit meta data", meta.RunnerName, meta.MetaFile(), err)
		return
	}
	tmps := strings.Split(sqlAndOffsets, sqlOffsetConnector)
	if int64(len(tmps)) != 2*length || int64(len(sqls)) != length {
		log.Errorf("Runner[%v] %v -meta file is not invalid sql meta file %v， omit meta data", meta.RunnerName, meta.MetaFile(), sqlAndOffsets)
		return
	}
	omitMeta = false
	offsets = make([]int64, length)
	for idx, sql := range sqls {
		syncSQL := strings.Replace(tmps[idx], "@", " ", -1)
		offset, err := strconv.ParseInt(tmps[idx+int(length)], 10, 64)
		if err != nil || sql != syncSQL {
			log.Errorf("Runner[%v] %v -meta file sql is out of date %v or parse offset err %v， omit this offset", meta.RunnerName, meta.MetaFile(), syncSQL, err)
		}
		offsets[idx] = offset
	}
	return
}

// 支持 YYYY, YY, MM, DD, hh, mm, ss, M, D, h, m, s
func convertMagic(magic string, now time.Time) (ret string) {
	switch magic {
	case "YYYY":
		return fmt.Sprintf("%d", now.Year())
	case "YY":
		return fmt.Sprintf("%d", now.Year())[2:]
	case "MM":
		m := int(now.Month())
		return fmt.Sprintf("%02d", m)
	case "M":
		m := int(now.Month())
		return fmt.Sprintf("%d", m)
	case "D":
		d := int(now.Day())
		return fmt.Sprintf("%d", d)
	case "DD":
		d := int(now.Day())
		return fmt.Sprintf("%02d", d)
	case "hh":
		h := now.Hour()
		return fmt.Sprintf("%02d", h)
	case "h":
		h := now.Hour()
		return fmt.Sprintf("%d", h)
	case "mm":
		m := now.Minute()
		return fmt.Sprintf("%02d", m)
	case "m":
		m := now.Minute()
		return fmt.Sprintf("%d", m)
	case "ss":
		s := now.Second()
		return fmt.Sprintf("%02d", s)
	case "s":
		s := now.Second()
		return fmt.Sprintf("%d", s)
	}
	return ""
}

// 仅支持 YYYY, YY, MM, DD, hh, mm, ss，不支持 M, D, h, m, s
func convertMagicIndex(magic string, now time.Time) (ret string, index int) {
	switch magic {
	case "YYYY":
		return fmt.Sprintf("%d", now.Year()), YEAR
	case "YY":
		return fmt.Sprintf("%d", now.Year())[2:], YEAR
	case "MM":
		m := int(now.Month())
		return fmt.Sprintf("%02d", m), MONTH
	case "DD":
		d := int(now.Day())
		return fmt.Sprintf("%02d", d), DAY
	case "hh":
		h := now.Hour()
		return fmt.Sprintf("%02d", h), HOUR
	case "mm":
		m := now.Minute()
		return fmt.Sprintf("%02d", m), MINUTE
	case "ss":
		s := now.Second()
		return fmt.Sprintf("%02d", s), SECOND
	}
	return "", -1
}

// 渲染魔法变量，获取渲染结果
func goMagic(rawSql string, now time.Time) (ret string) {
	sps := strings.Split(rawSql, "@(") //@()，对于每个分片找右括号
	ret = sps[0]
	for idx := 1; idx < len(sps); idx++ {
		idxr := strings.Index(sps[idx], ")")
		if idxr == -1 {
			return rawSql
		}
		ret += convertMagic(sps[idx][0:idxr], now)
		if idxr+1 < len(sps[idx]) {
			ret += sps[idx][idxr+1:]
		}
	}

	return ret
}

type MagicRes struct {
	timeStart   []int  // 按照 YY,MM,DD,hh,mm,ss 顺序记录时间位置
	timeEnd     []int  // 按照 YY,MM,DD,hh,mm,ss 顺序记录时间长度
	remainIndex []int  // 按顺序记录非时间字符串开始结束位置，去除 *
	ret         string // 渲染结果，包含 *
}

// 渲染魔法变量
func goMagicIndex(rawSql string, now time.Time) (MagicRes, error) {
	sps := strings.Split(rawSql, "@(") //@()，对于每个分片找右括号
	var magicRes = MagicRes{
		timeStart:   []int{-1, -1, -1, -1, -1, -1},
		timeEnd:     make([]int, 6),
		remainIndex: []int{0},
		ret:         sps[0],
	}
	recordIndex := len(magicRes.ret)

	// 没有魔法变量的情况，例如 mytest*
	if len(sps) < 2 {
		magicRes.remainIndex = append(magicRes.remainIndex, removeWildcards(magicRes.ret, recordIndex))
		return magicRes, nil
	}

	magicRes.remainIndex = append(magicRes.remainIndex, recordIndex)
	for idx := 1; idx < len(sps); idx++ {
		idxr := strings.Index(sps[idx], ")")
		if idxr == -1 {
			magicRes.ret = rawSql
			return magicRes, nil
		}
		spsStr := sps[idx][0:idxr]
		if len(spsStr) < 2 {
			magicRes.ret = rawSql
			return magicRes, fmt.Errorf(SupportReminder)
		}
		res, index := convertMagicIndex(sps[idx][0:idxr], now)
		if index == -1 {
			magicRes.ret = rawSql
			return magicRes, fmt.Errorf(SupportReminder)
		}

		// 记录日期起始位置
		magicRes.timeStart[index] = recordIndex
		magicRes.ret += res
		recordIndex = len(magicRes.ret)

		// 记录日期长度
		magicRes.timeEnd[index] = recordIndex

		if idxr+1 < len(sps[idx]) {
			spsRemain := sps[idx][idxr+1:]
			magicRes.ret += spsRemain
			if spsRemain == Wildcards {
				recordIndex = len(magicRes.ret)
				continue
			}
			magicRes.remainIndex = append(magicRes.remainIndex, recordIndex)
			magicRes.remainIndex = append(magicRes.remainIndex, removeWildcards(spsRemain, len(magicRes.ret)))
			recordIndex = len(magicRes.ret)
		}
	}

	return magicRes, nil
}

// 若包含通配符，字段长度相应 - 1
func removeWildcards(checkWildcards string, length int) int {
	if strings.Contains(checkWildcards, Wildcards) {
		return length - 1
	}
	return length
}

func checkMagic(rawSql string) (valid bool) {
	sps := strings.Split(rawSql, "@(") //@()，对于每个分片找右括号
	now := time.Now()

	for idx := 1; idx < len(sps); idx++ {
		idxr := strings.Index(sps[idx], ")")
		if idxr == -1 {
			return true
		}
		spsStr := sps[idx][0:idxr]
		if len(spsStr) < 2 {
			log.Errorf(SupportReminder)
			return false
		}

		_, index := convertMagicIndex(sps[idx][0:idxr], now)
		if index == -1 {
			log.Errorf(SupportReminder)
			return false
		}
	}

	return true
}

func updateSqls(rawsqls string, now time.Time) []string {
	encodedSQLs := strings.Split(rawsqls, sqlSpliter)
	sqls := make([]string, 0)
	for _, esql := range encodedSQLs {
		magicSQL := strings.TrimSpace(goMagic(esql, now))
		if len(magicSQL) <= 0 {
			continue
		}
		sqls = append(sqls, magicSQL)
	}
	return sqls
}

// check if syncSQLs is out of date
func (r *Reader) updateOffsets(sqls []string) {
	r.muxOffsets.Lock()
	defer r.muxOffsets.Unlock()
	for idx, sql := range sqls {
		if idx >= len(r.offsets) {
			r.offsets = append(r.offsets, 0)
			continue
		}
		if idx >= len(r.syncSQLs) {
			continue
		}
		if r.syncSQLs[idx] != sql {
			r.offsets[idx] = 0
		}
	}

	return
}

func (r *Reader) run() {
	// 未在准备状态（StatusInit）时无法执行此次任务
	if !atomic.CompareAndSwapInt32(&r.routineStatus, reader.StatusInit, reader.StatusRunning) {
		if r.isStopping() || r.hasStopped() {
			log.Warningf("Runner[%v] %q daemon has stopped, this task does not need to be executed and is skipped this time", r.meta.RunnerName, r.Name())
		} else {
			errMsg := fmt.Sprintf("Runner[%v] %q daemon is still working on last task, this task will not be executed and is skipped this time", r.meta.RunnerName, r.Name())
			log.Error(errMsg)
			if !r.isLoop {
				// 通知上层 Cron 执行间隔可能过短或任务执行时间过长
				r.sendError(errors.New(errMsg))
			}
		}
		return
	}
	defer func() {
		// 如果 reader 在 routine 运行时关闭，则需要此 routine 负责关闭数据管道
		if r.isStopping() || r.hasStopped() {
			if atomic.CompareAndSwapInt32(&r.routineStatus, reader.StatusRunning, reader.StatusStopping) {
				close(r.readChan)
				close(r.errChan)
			}
			return
		}
		atomic.StoreInt32(&r.routineStatus, reader.StatusInit)
	}()

	now := time.Now().Add(-r.magicLagDur)
	r.table = goMagic(r.rawTable, now)

	connectStr, err := r.getConnectStr("", now)
	if err != nil {
		log.Error(err)
		return
	}

	// 如果执行失败，最多重试 10 次
	for i := 1; i <= 10; i++ {
		// 判断上层是否已经关闭，先判断 routineStatus 再判断 status 可以保证同时只有一个 r.run 会运行到此处
		if r.isStopping() || r.hasStopped() {
			log.Warningf("Runner[%v] %q daemon has stopped, task is interrupted", r.meta.RunnerName, r.Name())
			return
		}

		err = r.exec(connectStr)
		if err == nil {
			log.Infof("Runner[%v] %q task has been successfully executed", r.meta.RunnerName, r.Name())
			return
		}

		log.Error(err)
		r.setStatsError(err.Error())
		r.sendError(err)

		if r.isLoop {
			return // 循环执行的任务上层逻辑已经等同重试
		}
		time.Sleep(3 * time.Second)
	}
	log.Errorf("Runner[%v] %q task execution failed and gave up after 10 tries", r.meta.RunnerName, r.Name())
}

func (r *Reader) getInitScans(length int, rows *sql.Rows, sqltype string) (scanArgs []interface{}, nochoiced []bool) {
	nochoice := make([]interface{}, length)
	nochoiced = make([]bool, length)
	for i := range scanArgs {
		nochoice[i] = new(interface{})
		nochoiced[i] = true
	}
	defer func() {
		if r := recover(); r != nil {
			log.Error("Recovered in f", r)
			scanArgs = nochoice
			return
		}
	}()

	tps, err := rows.ColumnTypes()
	if err != nil {
		log.Error(err)
		scanArgs = nochoice
	}
	if len(tps) != length {
		log.Errorf("Runner[%v] %v getInitScans length is %v not equal to columetypes %v", length, len(tps))
		scanArgs = nochoice
	}
	scanArgs = make([]interface{}, length)
	for i, v := range tps {
		nochoiced[i] = false
		scantype := v.ScanType().Name()
		switch scantype {
		case "int64", "int32", "int16", "int", "int8":
			scanArgs[i] = new(int64)
		case "float32", "float64":
			scanArgs[i] = new(float64)
		case "uint", "uint8", "uint16", "uint32", "uint64":
			scanArgs[i] = new(uint64)
		case "bool":
			scanArgs[i] = new(bool)
		case "[]uint8":
			scanArgs[i] = new([]byte)
		case "string", "RawBytes", "time.Time", "NullTime":
			//时间类型也作为string处理
			scanArgs[i] = new(interface{})
			if _, ok := r.schemas[v.Name()]; !ok {
				r.schemas[v.Name()] = "string"
			}
		case "NullInt64":
			scanArgs[i] = new(interface{})
			if _, ok := r.schemas[v.Name()]; !ok {
				r.schemas[v.Name()] = "long"
			}
		case "NullFloat64":
			scanArgs[i] = new(interface{})
			if _, ok := r.schemas[v.Name()]; !ok {
				r.schemas[v.Name()] = "float"
			}
		default:
			scanArgs[i] = new(interface{})
			nochoiced[i] = true
		}
		log.Infof("Runner[%v] %v Init field %v scan type is %v ", r.meta.RunnerName, r.Name(), v.Name(), scantype)
	}

	return scanArgs, nochoiced
}

func (r *Reader) getOffsetIndex(columns []string) int {
	offsetKeyIndex := -1
	for idx, key := range columns {
		if key == r.offsetKey {
			return idx
		}
	}
	return offsetKeyIndex
}

// mysql 中 若原始sql语句为空，则根据用户填写的database, table, historyAll获取数据，并且统计数据的条数
// 1. 若 database 和 table 都为空，则默认使用 *, 即获取所有的数据库和数据表
// 2. historyAll 为 true 时，获取所有小于渲染结果的数据
// 3. historyAll 为 false 时，若非定时任务，即只执行一次，获取与渲染结果相匹配的数据，若为定时任务，获取小于等于渲染结果的数据
func (r *Reader) exec(connectStr string) (err error) {
	now := time.Now().Add(-r.magicLagDur)
	// 获取符合条件的数据库
	dbs := make([]string, 0)
	switch r.dbtype {
	case reader.ModeMySQL:
		if r.rawSQLs != "" {
			dbs = append(dbs, goMagic(r.rawDatabase, now))
		} else {
			var err error
			dbs, err = r.getDBs(connectStr, now)
			if err != nil {
				return err
			}

			log.Infof("Runner[%v] %v get valid databases: %v", r.meta.RunnerName, r.Name(), dbs)

			go func() {
				// 获取数据库所有条数
				r.execDB(dbs, now, COUNTFUNC)
				return
			}()
		}
	case reader.ModeMSSQL, reader.ModePostgreSQL:
		dbs = append(dbs, r.database)
	}

	err = r.execDB(dbs, now, READFUNC)
	if err != nil {
		return err
	}

	return nil
}

func (r *Reader) execDB(dbs []string, now time.Time, handlerFunc int) error {
	for _, currentDB := range dbs {
		var recordTablesDone TableRecords
		tableRecords := r.doneRecords.GetTableRecords(currentDB)
		recordTablesDone.Set(tableRecords)

		switch handlerFunc {
		case COUNTFUNC:
			err := r.execCountDB(currentDB, now, recordTablesDone)
			if err != nil {
				log.Errorf("Runner[%v] %v get current database: %v count error: %v", r.meta.RunnerName, r.Name(), currentDB, err)
			}
		case READFUNC:
			err := r.execReadDB(currentDB, now, recordTablesDone)
			if err != nil {
				log.Errorf("Runner[%v] %v exect read db: %v error: %v,will retry read it", r.meta.RunnerName, r.Name(), currentDB, err)
				return err
			}
		}

		if r.isStopping() || r.hasStopped() {
			log.Warningf("Runner[%v] %v stopped from running", r.meta.RunnerName, currentDB)
			return nil
		}
	}
	return nil
}

func (r *Reader) execCountDB(curDB string, now time.Time, recordTablesDone TableRecords) error {
	connectStr, err := r.getConnectStr(curDB, now)
	if err != nil {
		return err
	}

	log.Infof("Runner[%v] prepare %v change database success, current database is: %v", r.meta.RunnerName, r.dbtype, curDB)

	//更新sqls
	var tables []string
	var sqls string
	if r.rawSQLs == "" {
		// 获取符合条件的数据表和count语句
		tables, sqls, err = r.getValidData(connectStr, curDB, r.rawTable, now, COUNT)
		if err != nil {
			return err
		}

		log.Infof("Runner[%v] %v default count sqls %v", r.meta.RunnerName, curDB, r.rawSQLs)

		if r.omitDoneDBRecords {
			// 兼容
			recordTablesDone.restoreTableDone(r.meta, curDB, tables)
		}
	}

	if r.rawSQLs != "" {
		sqls = r.rawSQLs
	}
	sqlsSlice := updateSqls(sqls, now)
	log.Infof("Runner[%v] %v start to work, sqls %v offsets %v", r.meta.RunnerName, curDB, sqlsSlice, r.offsets)
	tablesLen := len(tables)

	for idx, rawSql := range sqlsSlice {
		//分sql执行
		if r.rawSQLs == "" && idx < tablesLen {
			if recordTablesDone.GetTableInfo(tables[idx]) != (TableInfo{}) {
				continue
			}
		}

		// 每张表的记录数
		var tableSize int64
		tableSize, err = r.execTableCount(connectStr, idx, curDB, rawSql)
		if err != nil {
			return err
		}

		// 符合记录的数据库和表的记录总数
		r.addCount(tableSize)

		if r.isStopping() || r.hasStopped() {
			log.Warningf("Runner[%v] %v stopped from running", r.meta.RunnerName, curDB)
			return nil
		}
	}

	return nil
}

func (r *Reader) execReadDB(curDB string, now time.Time, recordTablesDone TableRecords) (err error) {
	connectStr, err := r.getConnectStr(curDB, now)
	if err != nil {
		return err
	}

	log.Infof("Runner[%v] %v prepare %v change database success", r.meta.RunnerName, curDB, r.dbtype)
	r.database = curDB

	//更新sqls
	var tables []string
	var sqls string
	if r.rawSQLs == "" {
		// 获取符合条件的数据表和获取所有数据的语句
		tables, sqls, err = r.getValidData(connectStr, curDB, r.rawTable, now, TABLE)
		if err != nil {
			log.Errorf("Runner[%v] %v rawTable: %v get tables and sqls error %v", r.meta.RunnerName, r.Name(), r.rawTable, r.rawSQLs, err)
			if len(tables) == 0 && sqls == "" {
				return err
			}
		}

		log.Infof("Runner[%v] %v default sqls %v", r.meta.RunnerName, r.Name(), sqls)

		if r.omitDoneDBRecords && !recordTablesDone.restoreTableDone(r.meta, curDB, tables) {
			// 兼容
			r.syncRecords.SetTableRecords(curDB, recordTablesDone)
			r.doneRecords.SetTableRecords(curDB, recordTablesDone)
		}
	}
	log.Infof("Runner[%v] %v get valid tables: %v, recordTablesDone: %v", r.meta.RunnerName, r.Name(), tables, recordTablesDone)

	var sqlsSlice []string
	if r.rawSQLs != "" {
		sqlsSlice = updateSqls(r.rawSQLs, now)
		r.updateOffsets(sqlsSlice)
	} else {
		sqlsSlice = updateSqls(sqls, now)
	}

	r.syncSQLs = sqlsSlice
	tablesLen := len(tables)
	log.Infof("Runner[%v] %v start to work, sqls %v offsets %v", r.meta.RunnerName, r.Name(), r.syncSQLs, r.offsets)

	for idx, rawSql := range r.syncSQLs {
		//分sql执行
		var isRawSql bool
		exit := false
		var tableName string
		var readSize int64
		for !exit {
			if r.rawSQLs == "" && idx < tablesLen {
				tableName = tables[idx]
				if recordTablesDone.GetTableInfo(tableName) != (TableInfo{}) {
					break
				}
			}
			// 执行每条 sql 语句
			exit, isRawSql, readSize, err = r.execReadSql(connectStr, curDB, idx, rawSql, tables)
			if err != nil {
				return err
			}

			if r.rawSQLs == "" {
				r.syncRecords.SetTableInfo(curDB, tableName, TableInfo{size: readSize, offset: -1})
				r.doneRecords.SetTableInfo(curDB, tableName, TableInfo{size: readSize, offset: -1})
				recordTablesDone.SetTableInfo(tableName, TableInfo{size: readSize, offset: -1})
			}

			if r.isStopping() || r.hasStopped() {
				log.Warningf("Runner[%v] %v stopped from running", r.meta.RunnerName, r.Name())
				return nil
			}

			if isRawSql {
				log.Infof("Runner[%v] %v is raw SQL, exit after exec once...", r.meta.RunnerName, r.Name())
				break
			}
		}
	}
	return nil
}

// WriteRecordsFile 将当前文件写入donefiel中
func WriteRecordsFile(doneFilePath, content string) (err error) {
	var f *os.File
	filename := fmt.Sprintf("%v.%v", reader.DoneFileName, DefaultDoneRecordsFile)
	filePath := filepath.Join(doneFilePath, filename)
	// write to tmp file
	f, err = os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, DefaultFilePerm)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write([]byte(content))
	if err != nil {
		return err
	}

	return f.Sync()
}

func convertLong(v interface{}) (int64, error) {
	dpv := reflect.ValueOf(v)
	if dpv.Kind() != reflect.Ptr {
		return 0, errors.New("scanArgs not a pointer")
	}
	if dpv.IsNil() {
		return 0, errors.New("scanArgs is a nil pointer")
	}
	dv := reflect.Indirect(dpv)
	switch dv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return dv.Int(), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int64(dv.Uint()), nil
	case reflect.String:
		return strconv.ParseInt(dv.String(), 10, 64)
	case reflect.Interface:
		idv := dv.Interface()
		if ret, ok := idv.(int64); ok {
			return ret, nil
		}
		if ret, ok := idv.(int); ok {
			return int64(ret), nil
		}
		if ret, ok := idv.(uint); ok {
			return int64(ret), nil
		}
		if ret, ok := idv.(uint64); ok {
			return int64(ret), nil
		}
		if ret, ok := idv.(string); ok {
			return strconv.ParseInt(ret, 10, 64)
		}
		if ret, ok := idv.(int8); ok {
			return int64(ret), nil
		}
		if ret, ok := idv.(int16); ok {
			return int64(ret), nil
		}
		if ret, ok := idv.(int32); ok {
			return int64(ret), nil
		}
		if ret, ok := idv.(uint8); ok {
			return int64(ret), nil
		}
		if ret, ok := idv.(uint16); ok {
			return int64(ret), nil
		}
		if ret, ok := idv.(uint32); ok {
			return int64(ret), nil
		}
		if ret, ok := idv.([]byte); ok {
			if len(ret) == 8 {
				return int64(binary.BigEndian.Uint64(ret)), nil
			} else {
				return strconv.ParseInt(string(ret), 10, 64)
			}
		}
		if idv == nil {
			return 0, nil
		}
		log.Errorf("sql reader convertLong for type %v is not supported", reflect.TypeOf(idv))
	}
	return 0, fmt.Errorf("%v type can not convert to int", dv.Kind())
}

func convertFloat(v interface{}) (float64, error) {
	dpv := reflect.ValueOf(v)
	if dpv.Kind() != reflect.Ptr {
		return 0, errors.New("scanArgs not a pointer")
	}
	if dpv.IsNil() {
		return 0, errors.New("scanArgs is a nil pointer")
	}
	dv := reflect.Indirect(dpv)
	switch dv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return float64(dv.Int()), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return float64(dv.Uint()), nil
	case reflect.Float32, reflect.Float64:
		return dv.Float(), nil
	case reflect.String:
		return strconv.ParseFloat(dv.String(), 64)
	case reflect.Interface:
		idv := dv.Interface()
		if ret, ok := idv.(float64); ok {
			return ret, nil
		}
		if ret, ok := idv.(float32); ok {
			return float64(ret), nil
		}
		if ret, ok := idv.(int64); ok {
			return float64(ret), nil
		}
		if ret, ok := idv.(int); ok {
			return float64(ret), nil
		}
		if ret, ok := idv.(uint); ok {
			return float64(ret), nil
		}
		if ret, ok := idv.(uint64); ok {
			return float64(ret), nil
		}
		if ret, ok := idv.(string); ok {
			return strconv.ParseFloat(ret, 64)
		}
		if ret, ok := idv.(int8); ok {
			return float64(ret), nil
		}
		if ret, ok := idv.(int16); ok {
			return float64(ret), nil
		}
		if ret, ok := idv.(int32); ok {
			return float64(ret), nil
		}
		if ret, ok := idv.(uint8); ok {
			return float64(ret), nil
		}
		if ret, ok := idv.(uint16); ok {
			return float64(ret), nil
		}
		if ret, ok := idv.(uint32); ok {
			return float64(ret), nil
		}
		if ret, ok := idv.([]byte); ok {
			return strconv.ParseFloat(string(ret), 64)
		}
		if idv == nil {
			return 0, nil
		}
		log.Errorf("sql reader convertFloat for type %v is not supported", reflect.TypeOf(idv))
	}
	return 0, fmt.Errorf("%v type can not convert to int", dv.Kind())
}

func convertString(v interface{}) (string, error) {
	dpv := reflect.ValueOf(v)
	if dpv.Kind() != reflect.Ptr {
		return "", errors.New("scanArgs not a pointer")
	}
	if dpv.IsNil() {
		return "", errors.New("scanArgs is a nil pointer")
	}
	dv := reflect.Indirect(dpv)
	switch dv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.Itoa(int(dv.Int())), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.Itoa(int(dv.Uint())), nil
	case reflect.String:
		return dv.String(), nil
	case reflect.Interface:
		idv := dv.Interface()
		if ret, ok := idv.(int64); ok {
			return strconv.Itoa(int(ret)), nil
		}
		if ret, ok := idv.(int); ok {
			return strconv.Itoa(int(ret)), nil
		}
		if ret, ok := idv.(uint); ok {
			return strconv.Itoa(int(ret)), nil
		}
		if ret, ok := idv.(uint64); ok {
			return strconv.Itoa(int(ret)), nil
		}
		if ret, ok := idv.(string); ok {
			return ret, nil
		}
		if ret, ok := idv.(int8); ok {
			return strconv.Itoa(int(ret)), nil
		}
		if ret, ok := idv.(int16); ok {
			return strconv.Itoa(int(ret)), nil
		}
		if ret, ok := idv.(int32); ok {
			return strconv.Itoa(int(ret)), nil
		}
		if ret, ok := idv.(uint8); ok {
			return strconv.Itoa(int(ret)), nil
		}
		if ret, ok := idv.(uint16); ok {
			return strconv.Itoa(int(ret)), nil
		}
		if ret, ok := idv.(uint32); ok {
			return strconv.Itoa(int(ret)), nil
		}
		if ret, ok := idv.([]byte); ok {
			return string(ret), nil
		}
		if idv == nil {
			return "", nil
		}
		log.Errorf("sql reader convertString for type %v is not supported", reflect.TypeOf(idv))
	}
	return "", fmt.Errorf("%v type can not convert to string", dv.Kind())
}

func (r *Reader) getSQL(idx int, rawSQL string) (sql string, err error) {
	r.muxOffsets.RLock()
	defer r.muxOffsets.RUnlock()
	rawSQL = strings.TrimSuffix(strings.TrimSpace(rawSQL), ";")
	switch r.dbtype {
	case reader.ModeMySQL:
		if len(r.offsetKey) > 0 && len(r.offsets) > idx {
			sql = fmt.Sprintf("%s WHERE %v >= %d AND %v < %d;", rawSQL, r.offsetKey, r.offsets[idx], r.offsetKey, r.offsets[idx]+int64(r.readBatch))
		} else {
			sql = fmt.Sprintf("%s", rawSQL)
		}
	case reader.ModeMSSQL:
		if len(r.offsetKey) > 0 && len(r.offsets) > idx {
			sql = fmt.Sprintf("%s WHERE CAST(%v AS BIGINT) >= %d AND CAST(%v AS BIGINT) < %d;", rawSQL, r.offsetKey, r.offsets[idx], r.offsetKey, r.offsets[idx]+int64(r.readBatch))
		} else {
			err = fmt.Errorf("%v dbtype is not support get SQL without id now", r.dbtype)
		}
	case reader.ModePostgreSQL:
		if len(r.offsetKey) > 0 && len(r.offsets) > idx {
			sql = fmt.Sprintf("%s WHERE %v >= %d AND %v < %d;", rawSQL, r.offsetKey, r.offsets[idx], r.offsetKey, r.offsets[idx]+int64(r.readBatch))
		} else {
			err = fmt.Errorf("%v dbtype is not support get SQL without id now", r.dbtype)
		}
	default:
		err = fmt.Errorf("%v dbtype is not support get SQL now", r.dbtype)
	}

	return sql, err
}

func (r *Reader) checkExit(idx int, db *sql.DB) (bool, int64) {
	if len(r.offsetKey) <= 0 {
		return true, -1
	}
	rawSQL := r.syncSQLs[idx]
	rawSQL = strings.TrimSuffix(strings.TrimSpace(rawSQL), ";")
	var tsql string
	if r.dbtype == reader.ModeMySQL {
		tsql = fmt.Sprintf("%s WHERE %v >= %d order by %v limit 1;", rawSQL, r.offsetKey, r.offsets[idx], r.offsetKey)
	} else {
		ix := strings.Index(rawSQL, "from")
		if ix < 0 {
			return true, -1
		}
		rawSQL = rawSQL[ix:]
		tsql = fmt.Sprintf("select top(1) * %v WHERE CAST(%v AS BIGINT) >= %v order by CAST(%v AS BIGINT);", rawSQL, r.offsetKey, r.offsets[idx], r.offsetKey)
	}
	rows, err := db.Query(tsql)
	if err != nil {
		log.Error(err)
		return true, -1
	}
	defer rows.Close()
	// Get column names
	columns, err := rows.Columns()
	if err != nil {
		log.Errorf("Runner[%v] %v prepare %v columns error %v", r.meta.RunnerName, r.Name(), r.dbtype, err)
		return true, -1
	}

	scanArgs, _ := r.getInitScans(len(columns), rows, r.dbtype)
	offsetKeyIndex := r.getOffsetIndex(columns)
	for rows.Next() {
		err = rows.Scan(scanArgs...)
		if err != nil {
			return false, -1
		}
		if offsetKeyIndex >= 0 {
			offsetIdx, err := convertLong(scanArgs[offsetKeyIndex])
			if err != nil {
				return false, -1
			}
			return false, offsetIdx
		}
		return false, -1
	}
	return true, -1
}

func contains(slice []string, str string) bool {
	for _, s := range slice {
		if s == str {
			return true
		}
	}
	return false
}

// 查看时间是否符合, min为true则取出来为小于等于，min为false则取出来大于等于
func compareTime(target, match string, timeStart, timeEnd []int, min bool) (valid bool) {
	for idx, record := range timeStart {
		if record == -1 {
			continue
		}

		if len(target) < timeEnd[idx] || len(match) < timeEnd[idx] {
			return false
		}

		// 比较大小
		curStr := target[record:timeEnd[idx]]
		curInt, err := strconv.Atoi(curStr)
		if err != nil {
			return false
		}
		matchStr := match[record:timeEnd[idx]]
		matchInt, err := strconv.Atoi(matchStr)
		if err != nil {
			return false
		}

		// 小于
		if curInt < matchInt {
			return min
		}

		if curInt > matchInt {
			return !min
		}

		// 相等
		valid = true
	}

	return true
}

// 查看时间是否相等
func equalTime(target, magicRet string, timeStart, timeEnd []int) (valid bool) {
	for idx, record := range timeStart {
		if record == -1 {
			continue
		}

		if len(target) < timeEnd[idx] {
			return false
		}

		// 比较大小
		curStr := target[record:timeEnd[idx]]
		curInt, err := strconv.Atoi(curStr)
		if err != nil {
			return false
		}
		matchStr := magicRet[record:timeEnd[idx]]
		matchInt, err := strconv.Atoi(matchStr)
		if err != nil {
			return false
		}

		// 等于
		if curInt != matchInt {
			return false
		}

		valid = true
	}

	return true
}

// 获取有效数据
func (r *Reader) getValidData(connectStr, curDB, rawData string, now time.Time, queryType int) (validData []string, sqls string, err error) {
	// 是否导入所有数据
	getAll, err := r.getAll(queryType)
	if err != nil {
		return nil, "", err
	}

	// get all databases and check validate database
	query, err := r.getQuery(queryType, curDB)
	if err != nil {
		return nil, "", err
	}

	db, err := openSql(r.dbtype, connectStr)
	if err != nil {
		return nil, "", err
	}
	defer func() {
		db.Close()
	}()
	if err = db.Ping(); err != nil {
		return nil, "", err
	}

	rowsDBs, err := db.Query(query)
	if err != nil {
		log.Errorf("Runner[%v] %v prepare %v <%v> query error %v", r.meta.RunnerName, curDB, r.dbtype, query, err)
		return nil, "", err
	}
	defer rowsDBs.Close()

	validData = make([]string, 0)
	for rowsDBs.Next() {
		var s string
		err = rowsDBs.Scan(&s)
		if err != nil {
			log.Errorf("Runner[%v] %v scan rows error %v", r.meta.RunnerName, curDB, err)
			continue
		}

		// queryType == TABLE时，检查是否已经读过，DATABASE 和 COUNT 不需要check
		if queryType == TABLE && r.checkDoneRecords(s, curDB) {
			continue
		}

		// 不导入所有数据时，需要进行匹配
		if !getAll {
			magicRes, err := goMagicIndex(rawData, now)
			if err != nil {
				return nil, "", err
			}

			magicRemainStr := getRemainStr(magicRes.ret, magicRes.remainIndex)
			if !r.compareData(queryType, curDB, s, magicRemainStr, &magicRes) {
				log.Infof("Runner[%v] %v current data: %v, current time data: %v, remain str: %v, timeIndex: %v", r.meta.RunnerName, curDB, s, magicRes.ret, magicRemainStr, magicRes.remainIndex)
				continue
			}
		}

		rawSql, err := r.getRawSqls(queryType, s)
		if err != nil {
			return validData, sqls, err
		}
		sqls += rawSql

		validData = append(validData, s)
	}

	return validData, sqls, nil
}

func (r *Reader) getConnectStr(database string, now time.Time) (connectStr string, err error) {
	switch r.dbtype {
	case reader.ModeMySQL:
		connectStr = r.datasource + "/" + database
		if r.encoder != "" {
			connectStr += "?charset=" + r.encoder
		}
	case reader.ModeMSSQL:
		r.database = goMagic(r.rawDatabase, now)
		connectStr = r.datasource + ";database=" + r.database
	case reader.ModePostgreSQL:
		r.database = goMagic(r.rawDatabase, now)
		spls := strings.Split(r.datasource, " ")
		contains := false
		for idx, v := range spls {
			if strings.Contains(v, "dbname") {
				contains = true
				spls[idx] = "dbname=" + r.database
			}
		}
		if !contains {
			spls = append(spls, "dbname="+r.database)
		}
		connectStr = strings.Join(spls, " ")
	default:
		return "", fmt.Errorf("not support reader type: %v", r.dbtype)
	}
	return connectStr, nil
}

func openSql(dbtype, connectStr string) (db *sql.DB, err error) {
	db, err = sql.Open(dbtype, connectStr)
	if err != nil {
		return nil, fmt.Errorf("open %v failed: %v", dbtype, err)
	}
	return db, nil
}

func compareRemainStr(target, magicRemainStr, magicRet string, magicRemainIndex []int) bool {
	if len(magicRemainIndex) > 0 && len(target) < magicRemainIndex[len(magicRemainIndex)-1] {
		return false
	}
	targetRemainStr := getRemainStr(target, magicRemainIndex)
	if len(targetRemainStr) < len(magicRemainStr) {
		return false
	}

	// magicRet 中如果含有通配符，则只需剩余字符匹配即可
	if targetRemainStr[:len(magicRemainStr)] != magicRemainStr {
		return false
	}

	// matchData中有通配符
	if strings.HasSuffix(magicRet, Wildcards) {
		return true
	}

	if len(target) > len(magicRet) {
		targetRemainStr += target[len(magicRet):]
		if targetRemainStr != magicRemainStr {
			return false
		}
	}

	return true
}

func getRemainStr(origin string, magicRemainIndex []int) (remainStr string) {
	if len(magicRemainIndex)%2 != 0 {
		return origin
	}

	for idx := 0; idx < len(magicRemainIndex); {
		remainStr += origin[magicRemainIndex[idx]:magicRemainIndex[idx+1]]
		idx = idx + 2
	}

	return remainStr
}

func (r *Reader) Lag() (rl *LagInfo, err error) {
	rl = &LagInfo{SizeUnit: "records"}
	if r.rawSQLs == "" {
		count := r.getCount()
		rl.Size = count - r.CurrentCount
		if rl.Size < 0 {
			rl.Size = 0
		}
		rl.Total = count
	}

	return rl, nil
}

func (r *Reader) getDefaultSql(database string) (defaultSql string, err error) {
	switch r.dbtype {
	case reader.ModeMySQL:
		return strings.Replace(DefaultMySQLTable, "DATABASE_NAME", database, -1), nil
	case reader.ModePostgreSQL:
		return strings.Replace(DefaultPGSQLTable, "SCHEMA_NAME", r.dbSchema, -1), nil
	case reader.ModeMSSQL:
		sql := strings.Replace(DefaultMSSQLTable, "DATABASE_NAME", database, -1)
		sql = strings.Replace(sql, "SCHEMA_NAME", r.dbSchema, -1)
		return sql, nil
	default:
		return "", fmt.Errorf("not support reader type: %v", r.dbtype)
	}
}

func (r *Reader) checkCron() bool {
	return r.isLoop || r.cronSchedule
}

// 是否获取所有数据
func (r *Reader) getAll(queryType int) (getAll bool, err error) {
	switch queryType {
	case TABLE, COUNT:
		return r.rawTable == "*", nil
	case DATABASE:
		return r.rawDatabase == "*", nil
	default:
		return false, fmt.Errorf("%v queryType is not support get sql now", queryType)
	}

	return true, nil
}

// 根据数据库类型返回表名
func (r *Reader) getWrappedTableName(table string) (tableName string, err error) {
	switch r.dbtype {
	case reader.ModeMySQL:
		tableName = "`" + table + "`"
	case reader.ModeMSSQL:
		tableName = fmt.Sprintf("\"%s\".\"%s\"", r.dbSchema, table)
	case reader.ModePostgreSQL:
		tableName = fmt.Sprintf("\"%s\".\"%s\"", r.dbSchema, table)
	default:
		err = fmt.Errorf("%v mode not support in sql reader", r.dbtype)
	}
	return tableName, nil
}

// 根据 queryType 获取 table 中所有记录或者表中所有数据的条数的sql语句
func (r *Reader) getRawSqls(queryType int, table string) (sqls string, err error) {
	switch queryType {
	case TABLE:
		tableName, err := r.getWrappedTableName(table)
		if err != nil {
			return "", err
		}
		sqls += "Select * From " + tableName + ";"
	case COUNT:
		tableName, err := r.getWrappedTableName(table)
		if err != nil {
			return "", err
		}
		sqls += "Select Count(*) From " + tableName + ";"
	case DATABASE:
	default:
		return "", fmt.Errorf("%v queryType is not support get sql now", queryType)
	}

	return sqls, nil
}

// 根据 queryType 获取query语句
func (r *Reader) getQuery(queryType int, curDB string) (query string, err error) {
	switch queryType {
	case TABLE, COUNT:
		return r.getDefaultSql(curDB)
	case DATABASE:
		return DefaultMySQLDatabase, nil
	default:
		return "", fmt.Errorf("%v queryType is not support get sql now", queryType)
	}
}

// 计算每个table的记录条数
func (r *Reader) execTableCount(connectStr string, idx int, curDB, rawSql string) (tableSize int64, err error) {
	execSQL, err := r.getSQL(idx, rawSql)
	if err != nil {
		log.Errorf("Runner[%v] get SQL error %v, use raw SQL", r.meta.RunnerName, err)
		execSQL = rawSql
	}
	log.Infof("Runner[%v] reader <%v> exec sql <%v>", r.meta.RunnerName, curDB, execSQL)

	db, err := openSql(r.dbtype, connectStr)
	if err != nil {
		return 0, err
	}
	defer func() {
		db.Close()
	}()
	if err = db.Ping(); err != nil {
		return 0, err
	}

	rows, err := db.Query(execSQL)
	if err != nil {
		log.Errorf("Runner[%v] %v prepare %v <%v> query error %v", r.meta.RunnerName, curDB, r.dbtype, execSQL, err)
		return 0, err
	}
	defer rows.Close()

	// Fetch rows
	for rows.Next() {
		var s string
		err = rows.Scan(&s)
		if err != nil {
			log.Errorf("Runner[%v] %v scan rows error %v", r.meta.RunnerName, curDB, err)
			return 0, err
		}

		tableSize, err = strconv.ParseInt(s, 10, 64)
		if err != nil {
			log.Errorf("Runner[%v] %v convert string to int64 error %v", r.meta.RunnerName, curDB, err)
			return 0, err
		}
	}

	return tableSize, nil
}

// 执行每条 sql 语句
func (r *Reader) execReadSql(connectStr, curDB string, idx int, rawSql string, tables []string) (exit bool, isRawSql bool, readSize int64, err error) {
	exit = true

	execSQL, err := r.getSQL(idx, r.syncSQLs[idx])
	if err != nil {
		log.Warningf("Runner[%v] get SQL error %v, use raw SQL", r.meta.RunnerName, err)
		execSQL = rawSql
	}

	if execSQL == rawSql {
		isRawSql = true
	}

	db, err := openSql(r.dbtype, connectStr)
	if err != nil {
		return exit, isRawSql, 0, err
	}
	defer func() {
		db.Close()
	}()
	if err = db.Ping(); err != nil {
		return exit, isRawSql, 0, err
	}

	log.Infof("Runner[%v] reader <%v> exec sql <%v>", r.meta.RunnerName, r.Name(), execSQL)
	rows, err := db.Query(execSQL)
	if err != nil {
		err = fmt.Errorf("runner[%v] %v prepare %v <%v> query error %v", r.meta.RunnerName, r.Name(), r.dbtype, execSQL, err)
		log.Error(err)
		r.sendError(err)
		return exit, isRawSql, readSize, err
	}
	defer rows.Close()
	// Get column names
	columns, err := rows.Columns()
	if err != nil {
		err = fmt.Errorf("runner[%v] %v prepare %v <%v> columns error %v", r.meta.RunnerName, r.Name(), r.dbtype, execSQL, err)
		log.Error(err)
		r.sendError(err)
		return exit, isRawSql, readSize, err
	}
	log.Infof("Runner[%v] SQL ：<%v>, schemas: <%v>", r.meta.RunnerName, execSQL, strings.Join(columns, ", "))
	scanArgs, nochiced := r.getInitScans(len(columns), rows, r.dbtype)
	var offsetKeyIndex int
	if r.rawSQLs != "" {
		offsetKeyIndex = r.getOffsetIndex(columns)
	}

	// Fetch rows
	var maxOffset int64 = -1
	for rows.Next() {
		exit = false
		// get RawBytes from data
		err = rows.Scan(scanArgs...)
		if err != nil {
			err = fmt.Errorf("runner[%v] %v scan rows error %v", r.meta.RunnerName, r.Name(), err)
			log.Error(err)
			r.sendError(err)
			continue
		}

		var totalBytes int64
		data := make(Data, len(scanArgs))
		for i := 0; i < len(scanArgs); i++ {
			var bytes int64
			vtype, ok := r.schemas[columns[i]]
			if !ok {
				vtype = "unknown"
			}
			switch vtype {
			case "long":
				val, serr := convertLong(scanArgs[i])
				if serr != nil {
					serr = fmt.Errorf("runner[%v] %v convertLong for %v (%v) error %v, this key will be ignored", r.meta.RunnerName, r.Name(), columns[i], scanArgs[i], serr)
					log.Error(serr)
					r.sendError(serr)
				} else {
					data[columns[i]] = val
					bytes = 8
				}
			case "float":
				val, serr := convertFloat(scanArgs[i])
				if serr != nil {
					serr = fmt.Errorf("runner[%v] %v convertFloat for %v (%v) error %v, this key will be ignored", r.meta.RunnerName, r.Name(), columns[i], scanArgs[i], serr)
					log.Error(serr)
					r.sendError(serr)
				} else {
					data[columns[i]] = val
					bytes = 8
				}
			case "string":
				val, serr := convertString(scanArgs[i])
				if serr != nil {
					serr = fmt.Errorf("runner[%v] %v convertString for %v (%v) error %v, this key will be ignored", r.meta.RunnerName, r.Name(), columns[i], scanArgs[i], serr)
					log.Error(serr)
					r.sendError(serr)
				} else {
					data[columns[i]] = val
					bytes = int64(len(val))
				}
			default:
				dealed := false
				if !nochiced[i] {
					dealed = true
					switch d := scanArgs[i].(type) {
					case *string:
						data[columns[i]] = *d
						bytes = int64(len(*d))
					case *[]byte:
						data[columns[i]] = string(*d)
						bytes = int64(len(*d))
					case *bool:
						data[columns[i]] = *d
						bytes = 1
					case int64:
						data[columns[i]] = d
						bytes = 8
					case *int64:
						data[columns[i]] = *d
						bytes = 8
					case float64:
						data[columns[i]] = d
						bytes = 8
					case *float64:
						data[columns[i]] = *d
						bytes = 8
					case uint64:
						data[columns[i]] = d
						bytes = 8
					case *uint64:
						data[columns[i]] = *d
						bytes = 8
					case *interface{}:
						dealed = false
					default:
						dealed = false
					}
				}
				if !dealed {
					val, serr := convertString(scanArgs[i])
					if serr != nil {
						serr = fmt.Errorf("runner[%v] %v convertString for %v (%v) error %v, this key will be ignored", r.meta.RunnerName, r.Name(), columns[i], scanArgs[i], serr)
						log.Error(serr)
						r.sendError(serr)
					} else {
						data[columns[i]] = val
						bytes = int64(len(val))
					}
				}
			}

			totalBytes += bytes
		}
		if r.isStopping() || r.hasStopped() {
			log.Warningf("Runner[%v] %v stopped from running", r.meta.RunnerName, r.Name())
			return exit, isRawSql, readSize, nil
		}
		r.readChan <- readInfo{data, totalBytes}
		r.CurrentCount++
		readSize++

		if r.historyAll || r.rawSQLs == "" {
			continue
		}

		maxOffset = r.updateOffset(idx, offsetKeyIndex, maxOffset, scanArgs)
	}

	if maxOffset > 0 {
		r.offsets[idx] = maxOffset + 1
	}
	if exit {
		var newOffsetIdx int64
		exit, newOffsetIdx = r.checkExit(idx, db)
		if !exit {
			r.offsets[idx] += int64(r.readBatch)
			if newOffsetIdx > r.offsets[idx] {
				r.offsets[idx] = newOffsetIdx
			}
		} else {
			log.Infof("Runner[%v] %v no data any more, exit...", r.meta.RunnerName, r.Name())
		}
	}

	return exit, isRawSql, readSize, rows.Err()
}

func (r *Reader) updateOffset(idx, offsetKeyIndex int, maxOffset int64, scanArgs []interface{}) int64 {
	if offsetKeyIndex >= 0 {
		var tmpOffsetIndex int64
		tmpOffsetIndex, err := convertLong(scanArgs[offsetKeyIndex])
		if err != nil {
			log.Errorf("Runner[%v] %v offset key value parse error %v, offset was not recorded", r.meta.RunnerName, r.Name(), err)
		} else if tmpOffsetIndex > maxOffset {
			maxOffset = tmpOffsetIndex
		}
	} else {
		r.muxOffsets.Lock()
		r.offsets[idx]++
		r.muxOffsets.Unlock()
	}

	return maxOffset
}

func (r *Reader) addCount(current int64) {
	r.countLock.Lock()
	defer r.countLock.Unlock()
	r.count += current
}

func (r *Reader) getCount() int64 {
	r.countLock.RLock()
	defer r.countLock.RUnlock()
	return r.count
}

func (r *Reader) checkDoneRecords(target, curDB string) bool {
	var tableInfo TableInfo
	tableDoneRecords := r.doneRecords.GetTableRecords(curDB)
	if tableDoneRecords.GetTable() == nil {
		return false
	}

	tableInfo = tableDoneRecords.GetTableInfo(target)
	if tableInfo == (TableInfo{}) {
		return false
	}

	return true
}

// 取大于等于 最后一条记录 的数据，结果为 true 为小于或者不符合, false 为大于等于
func (r *Reader) greaterThanLastRecord(queryType int, target, magicRemainStr string, magicRes *MagicRes) bool {
	log.Infof("Runner[%v] current data: %v, last database record: %v, last table record: %v", r.meta.RunnerName, target, r.lastDatabase, r.lastTabel)
	if magicRes == nil {
		return true
	}
	var rawData string
	switch queryType {
	case DATABASE:
		rawData = r.lastDatabase
	case TABLE, COUNT:
		rawData = r.lastTabel
	default:
		return false
	}

	if len(rawData) == 0 {
		return true
	}
	log.Infof("Runner[%v] last %v is: %v, target: %v, magicRes: %v", r.meta.RunnerName, queryType, rawData, target, magicRes)

	match := compareRemainStr(rawData, magicRemainStr, magicRes.ret, magicRes.remainIndex)
	if !match {
		return false
	}

	return compareTime(target, rawData, magicRes.timeStart, magicRes.timeEnd, false)
}

func (r *Reader) getDBs(connectStr string, now time.Time) ([]string, error) {
	db, err := openSql(r.dbtype, connectStr)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err = db.Ping(); err != nil {
		return nil, err
	}
	// 获取所有符合条件的数据库
	dbsAll, _, err := r.getValidData(connectStr, "", r.rawDatabase, now, DATABASE)
	if err != nil {
		return dbsAll, err
	}

	dbs := make([]string, 0)
	for _, db := range dbsAll {
		if contains(MysqlSystemDB, strings.ToLower(db)) {
			continue
		}
		dbs = append(dbs, db)
	}

	return dbs, nil
}

// 不为 * 时，进行匹配
func (r *Reader) compareData(queryType int, curDB, target, magicRemainStr string, magicRes *MagicRes) bool {
	if magicRes == nil {
		return true
	}

	match := compareRemainStr(target, magicRemainStr, magicRes.ret, magicRes.remainIndex)
	log.Infof("Runner[%v] %v current data: %v, current time data: %v, remain str: %v, magicRemainIndex: %v, isMatch: %v", r.meta.RunnerName, curDB, target, magicRes.ret, magicRemainStr, magicRes.remainIndex, match)
	if !match {
		return false
	}

	if r.historyAll {
		// 取大于等于上一条的和小于等于现有的
		if compareTime(target, magicRes.ret, magicRes.timeStart, magicRes.timeEnd, true) &&
			r.greaterThanLastRecord(queryType, target, magicRemainStr, magicRes) {
			return true
		}
	} else {
		// 执行一次，应符合渲染结果
		if !r.checkCron() {
			return equalTime(target, magicRes.ret, magicRes.timeStart, magicRes.timeEnd)
		}

		// 取大于等于上一条的和小于等于现有的
		if compareTime(target, magicRes.ret, magicRes.timeStart, magicRes.timeEnd, true) &&
			r.greaterThanLastRecord(queryType, target, magicRemainStr, magicRes) {
			return true
		}
	}

	return false
}
