package mgr

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/longxiucai/logkit/cleaner"
	config "github.com/longxiucai/logkit/conf"
	"github.com/longxiucai/logkit/parser"
	"github.com/longxiucai/logkit/reader"
	"github.com/longxiucai/logkit/sender"
	. "github.com/longxiucai/logkit/utils/models"
	utilsos "github.com/longxiucai/logkit/utils/os"

	log "k8s.io/klog/v2"

	"github.com/howeyc/fsnotify"
	jsoniter "github.com/json-iterator/go"
)

var DIR_NOT_EXIST_SLEEP_TIME = "300" //300 s
var DEFAULT_LOGKIT_REST_DIR = "/.logkitconfs"

type ManagerConfig struct {
	BindHost string `json:"bind_host"`

	Idc          string        `json:"idc"`
	Zone         string        `json:"zone"`
	RestDir      string        `json:"rest_dir"`
	Cluster      ClusterConfig `json:"cluster"`
	DisableWeb   bool          `json:"disable_web"`
	ServerBackup bool          `json:"-"`
}

type cleanQueue struct {
	cleanerCount int
	filecount    map[string]int
	key          string //where this queue is stored
}

type Manager struct {
	ManagerConfig
	DefaultDir  string
	cleanLock   *sync.RWMutex
	watcherMux  *sync.RWMutex
	cleanChan   chan cleaner.CleanSignal
	cleanQueues map[string]*cleanQueue

	// runnerLock 锁住runner的状态，即 map runners、runnerNames 和 runnerConfigs 的情况有变动就要锁住
	runnerLock sync.RWMutex
	// runners  存储了正在运行的 runner
	runners map[string]Runner
	// runnerNames 存储了当前已经被使用的 runner name
	runnerNames map[string]bool
	// runnerConfigs 存储了当前每个 runner 对应的 config
	runnerConfigs map[string]RunnerConfig

	watchers  map[string]*fsnotify.Watcher // inode到watcher的映射表
	rregistry *reader.Registry
	pregistry *parser.Registry
	sregistry *sender.Registry

	Version    string
	SystemInfo string
}

func NewManager(conf ManagerConfig) (*Manager, error) {
	rr := reader.NewRegistry()
	pr := parser.NewRegistry()
	sr := sender.NewRegistry()
	return NewCustomManager(conf, rr, pr, sr)
}

func NewCustomManager(conf ManagerConfig, rr *reader.Registry, pr *parser.Registry, sr *sender.Registry) (*Manager, error) {
	if conf.RestDir == "" {
		dir, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("get system current workdir error %v, please set rest_dir config", err)
		}
		conf.RestDir = dir + DEFAULT_LOGKIT_REST_DIR
	} else {
		var err error
		if conf.RestDir, err = filepath.Abs(conf.RestDir); err != nil {
			return nil, err
		}
	}
	if !conf.ServerBackup {
		if err := os.MkdirAll(conf.RestDir, DefaultDirPerm); err != nil && !os.IsExist(err) {
			log.Warningf("make dir for rest default dir error %v", err)
		}
	}
	m := &Manager{
		ManagerConfig: conf,
		cleanLock:     new(sync.RWMutex),
		watcherMux:    new(sync.RWMutex),
		cleanChan:     make(chan cleaner.CleanSignal),
		cleanQueues:   make(map[string]*cleanQueue),
		runners:       make(map[string]Runner),
		runnerConfigs: make(map[string]RunnerConfig),
		runnerNames:   make(map[string]bool),
		watchers:      make(map[string]*fsnotify.Watcher),
		rregistry:     rr,
		pregistry:     pr,
		sregistry:     sr,
		SystemInfo:    utilsos.GetOSInfo().String(),
	}
	return m, nil
}

func (m *Manager) Stop() error {
	m.runnerLock.Lock()
	for _, runner := range m.runners {
		runner.Stop()
		runnerStatus, ok := runner.(StatusPersistable)
		if ok {
			runnerStatus.StatusBackup()
		}
	}
	m.runnerLock.Unlock()

	m.watcherMux.Lock()
	for _, w := range m.watchers {
		if w != nil {
			w.Close()
		}
	}
	m.watcherMux.Unlock()
	close(m.cleanChan)
	return nil
}

func (m *Manager) RemoveWithConfig(confPath string, isDelete bool) (err error) {
	if !strings.HasSuffix(confPath, ".conf") {
		err = fmt.Errorf("%v not end with .conf, skipped", confPath)
		log.Warning(err)
		return
	}
	log.Info("try remove", confPath)
	confPathAbs, err := filepath.Abs(confPath)
	if err != nil {
		err = fmt.Errorf("filepath.Abs(%s) failed: %v", confPath, err)
		log.Warning(err)
		return
	}
	confPath = confPathAbs
	m.runnerLock.Lock()
	defer m.runnerLock.Unlock()

	runner, ok := m.runners[confPath]
	if !ok {
		err = fmt.Errorf("%s not added, nothing to do", confPath)
		log.Warning(err)
		return
	}

	m.removeCleanQueue(runner.Cleaner())
	runner.Stop()
	delete(m.runners, confPath)
	delete(m.runnerNames, runner.Name())
	if isDelete {
		delete(m.runnerConfigs, confPath)
	}
	log.Infof("runner %s be removed, total %d", runner.Name(), len(m.runners))
	if runnerStatus, ok := runner.(StatusPersistable); ok {
		runnerStatus.StatusBackup()
	}
	return
}

func (m *Manager) Remove(confPath string) (err error) {
	return m.RemoveWithConfig(confPath, true)
}

func (m *Manager) addCleanQueue(info CleanInfo) {
	if !info.enable {
		return
	}
	m.cleanLock.Lock()
	defer m.cleanLock.Unlock()
	cq, ok := m.cleanQueues[info.logdir]
	if ok {
		cq.cleanerCount++
	} else {
		cq = &cleanQueue{
			cleanerCount: 1,
			filecount:    make(map[string]int),
			key:          info.logdir,
		}
	}
	log.Info(">>>>>>>>>>>> add clean queue", cq.cleanerCount, info.logdir)
	m.cleanQueues[info.logdir] = cq
	return
}

func (m *Manager) removeCleanQueue(info CleanInfo) {
	if !info.enable {
		return
	}
	m.cleanLock.Lock()
	defer m.cleanLock.Unlock()
	cq, ok := m.cleanQueues[info.logdir]
	if !ok {
		log.Errorf("can't find clean queue %v to remove", info.logdir)
		return
	}
	cq.cleanerCount--
	if cq.cleanerCount <= 0 {
		delete(m.cleanQueues, info.logdir)
	}
	log.Info(">>>>>>>>>>>> remove clean queue", cq.cleanerCount, info.logdir)
	return
}

func (m *Manager) Add(confPath string) {
	if !strings.HasSuffix(confPath, ".conf") {
		log.Warningf("Config %q does not end with '.conf', skipped", confPath)
		return
	}
	log.Infof("Adding config %q", confPath)
	confPathAbs, _, err := GetRealPath(confPath)
	if err != nil {
		log.Warningf("Failed to get real path of %q: %v", confPath, err)
		return
	}
	confPath = confPathAbs
	if m.IsRunning(confPath) {
		log.Errorf("Config %q has already been added", confPath)
		return
	}
	var conf RunnerConfig
	err = config.LoadEx(&conf, confPath)
	if err != nil {
		log.Warningf("Failed to load config %q: %v", confPath, err)
		return
	}

	modTime := time.Now()
	fi, err := os.Stat(confPath)
	if err != nil {
		log.Warningf("Failed to get config modtime: %v", err)
	} else {
		modTime = fi.ModTime()
	}

	log.Infof("Adding runner %q", conf.RunnerName)
	conf.CreateTime = modTime.Format(time.RFC3339Nano)
	go func() {
		if err := m.ForkRunner(confPath, conf, false); err != nil {
			log.Errorf("Failed to add runner %q: %v", confPath, err)
		}
	}()
}

func (m *Manager) ForkRunner(confPath string, config RunnerConfig, returnOnErr bool) error {
	var runner Runner
	var err error
	i := 0
	for {
		if m.IsRunning(confPath) {
			err = fmt.Errorf("%s already added - ", confPath)
			if !returnOnErr {
				log.Error(err)
			}
			return err
		}
		if config.IsStopped {
			m.runnerLock.Lock()
			m.runnerConfigs[confPath] = config
			m.runnerLock.Unlock()
			return nil
		}
		for k := range config.SendersConfig {
			var webornot string
			if config.IsInWebFolder {
				webornot = "Web"
			} else {
				webornot = "Terminal"
			}
			if config.SendersConfig[k] == nil {
				return fmt.Errorf("%s sender config is invalid", confPath)
			}
			config.SendersConfig[k][sender.InnerUserAgent] = "logkit/" + m.Version + " " + m.SystemInfo + " " + webornot
		}

		if runner, err = NewCustomRunner(config, m.cleanChan, m.rregistry, m.pregistry, m.sregistry); err != nil {
			errVal, ok := err.(*os.PathError)
			if !ok {
				err = fmt.Errorf("NewRunner(%v) failed: %v", config.RunnerName, err)
				if !returnOnErr {
					log.Error(err)
				}
				return err
			}
			if returnOnErr {
				return fmt.Errorf("NewRunner(%v) failed: os.PathError %v", config.RunnerName, err)
			}
			i++
			log.Warningf("LogDir(%v) does not exsit after %d rounds, sleep 5 minute and try again...", errVal.Path, i)
			sleepTimeStr := os.Getenv("DIR_NOT_EXIST_SLEEP_TIME")
			if sleepTimeStr == "" {
				sleepTimeStr = DIR_NOT_EXIST_SLEEP_TIME
			}
			sleepTime, _ := strconv.ParseInt(sleepTimeStr, 10, 0)
			time.Sleep(time.Duration(sleepTime) * time.Second)
			continue
		}
		break
	}
	m.runnerLock.Lock()
	defer m.runnerLock.Unlock()
	// 确保 config 没有重复添加，且 runner name 没有冲突
	if _, ok := m.runners[confPath]; ok {
		return fmt.Errorf("config path %q already added", confPath)
	} else if m.runnerNames[config.RunnerName] {
		return fmt.Errorf("runner name %q already used", config.RunnerName)
	}

	m.addCleanQueue(runner.Cleaner())
	log.Infof("Runner[%v] added: %#v", config.RunnerName, confPath)
	go runner.Run()
	m.runners[confPath] = runner
	m.runnerNames[config.RunnerName] = true
	m.runnerConfigs[confPath] = config
	log.Infof("new Runner[%v] is added, total %d", config.RunnerName, len(m.runners))
	return nil
}

func (m *Manager) IsRunning(confPath string) bool {
	_, ok := m.readRunners(confPath)
	if ok {
		return true
	}
	return false
}

// 创建文件会触发 create和modify事件
// 重命名会触发 rename和create事件
// 删除会触发 delete事件
// 修改会触发 delete事件、create事件、modify事件以及modify|ATTRIB事件
func (m *Manager) handle(path string, watcher *fsnotify.Watcher) {
	for {
		select {
		case ev, ok := <-watcher.Event:
			if !ok {
				log.Info("Manager watcher chan was closed")
				return
			}
			log.Info("event:", ev)
			if ev.IsDelete() || ev.IsRename() {
				_, err := os.Stat(path)
				if os.IsNotExist(err) {
					// 如果当前监听文件被删除，则不再监听，退出
					log.Warningf("close file watcher path %v", path)
					m.watcherMux.Lock()
					watcher.Close()
					delete(m.watchers, path)
					m.watcherMux.Unlock()
					// TODO 此处代表文件夹被删了，只移除一个runner可能不够，文件夹下会有其他runner没有被删除
					m.Remove(ev.Name)
					return
				}
				m.Remove(ev.Name)
			}
			if ev.IsCreate() {
				m.Add(ev.Name)
			}
			if ev.IsModify() && !ev.IsCreate() {
				m.Remove(ev.Name)
				m.Add(ev.Name)
			}
		case err := <-watcher.Error:
			if err != nil {
				log.Error("error:", err)
			}
		}
	}
}

func (m *Manager) getCleanQueues(dir, file, mode string) ([]*cleanQueue, error) {
	if mode == reader.ModeTailx {
		cleanQueues := make([]*cleanQueue, 0, len(m.cleanQueues))
		for k, v := range m.cleanQueues {
			matched, err := filepath.Match(k, filepath.Join(dir, file))
			if err != nil {
				log.Errorf("match pattern[%v] to path(%v) err %v", k, filepath.Join(dir, file), err)
				continue
			}
			if matched {
				cleanQueues = append(cleanQueues, v)
			}
		}
		return cleanQueues, nil
	}

	q, ok := m.cleanQueues[dir]
	if !ok {
		return nil, fmt.Errorf("cleaner dir %v not exist but got clean signal for delete file %v", dir, file)
	}
	return []*cleanQueue{q}, nil
}

func (m *Manager) doClean(sig cleaner.CleanSignal) {
	m.cleanLock.Lock()
	defer m.cleanLock.Unlock()
	dir, _, err := GetRealPath(sig.Logdir)
	if err != nil {
		log.Errorf("get GetRealPath for %v error %v", dir, err)
		return
	}
	file := sig.Filename
	queues, err := m.getCleanQueues(dir, file, sig.ReadMode)
	if err != nil {
		log.Error(sig.Cleaner, err)
		return
	}
	//check if all queues can be cleaned
	var canBeDeleted = true
	for _, q := range queues {
		count := q.filecount[file] + 1
		if count < q.cleanerCount {
			canBeDeleted = false
		}
		q.filecount[file] = count
	}
	if canBeDeleted {
		catdir := filepath.Join(dir, file)
		err := os.Remove(catdir)
		if err != nil {
			if os.IsNotExist(err) {
				log.Warningf("clean %v failed as logfile is not exist: %v", catdir, err)
			} else {
				log.Errorf("clean %v failed: %v", catdir, err)
			}
		} else {
			log.Infof("log <%v> was successfully cleaned by cleaner", catdir)
		}
		for _, q := range queues {
			delete(q.filecount, file)
		}
	}
	for _, q := range queues {
		m.cleanQueues[q.key] = q
	}
	return
}

func (m *Manager) clean() {
	for sig := range m.cleanChan {
		m.doClean(sig)
	}
}

func (m *Manager) detectMoreWatchers(confsPath []string) {
	ticker := time.NewTicker(time.Second * 10)
	for {
		select {
		case <-ticker.C:
			m.watcherMux.Lock()
			watcherNum := len(m.watchers)
			m.watcherMux.Unlock()
			log.Infof("we have totally %v watchers, periodically try to detect more watchers...", watcherNum)
			m.addWatchers(confsPath)
		}
	}
}

func (m *Manager) addWatchers(confsPath []string) (err error) {
	for _, dir := range confsPath {
		paths, err := filepath.Glob(dir)
		if err != nil {
			log.Errorf("filepath.Glob(%s): %v, err:%v", dir, paths, err)
			continue
		}
		if len(paths) <= 0 {
			log.Infof("confPath Config %v can not find any real conf dir", dir)
		}
		for _, path := range paths {
			m.watcherMux.RLock()
			_, exist := m.watchers[path]
			m.watcherMux.RUnlock()
			if exist {
				// 如果文件已经被监听，则不再重复监听
				continue
			}
			files, err := ioutil.ReadDir(path)
			if err != nil {
				log.Errorf("ioutil.ReadDir(%s): %v, err:%v", path, files, err)
				continue
			}
			log.Warningf("start to add watcher of conf path %v", path)
			for _, f := range files {
				if f.IsDir() {
					log.Warningf("skipped dir %v", f.Name())
					continue
				}
				m.Add(filepath.Join(path, f.Name()))
			}

			// Note: fsnotify has potential data race when New/Close watchers
			m.watcherMux.Lock()
			watcher, err := fsnotify.NewWatcher()
			if err != nil {
				m.watcherMux.Unlock()
				log.Errorf("fsnotify.NewWatcher: %v", err)
				continue
			}
			m.watchers[path] = watcher
			m.watcherMux.Unlock()
			go m.handle(path, watcher)
			if err = watcher.Watch(path); err != nil {
				log.Errorf("watch %v error %v", path, err)
			}
		}
	}
	return nil
}

func (m *Manager) Watch(confsPath []string) (err error) {
	err = m.addWatchers(confsPath)
	if err != nil {
		log.Errorf("addWatchers error : %v", err)
	}
	go m.detectMoreWatchers(confsPath)
	go m.clean()
	return
}

func (m *Manager) RestoreWebDir() {
	files, err := ioutil.ReadDir(m.RestDir)
	if err != nil {
		log.Errorf("ioutil.ReadDir(%s): %v, err:%v", m.RestDir, files, err)
		return
	}
	nums := 0
	for _, f := range files {
		if f.IsDir() {
			log.Info("skipped dir", f.Name())
			continue
		}
		m.Add(filepath.Join(m.RestDir, f.Name()))
		nums++
	}
	log.Infof("successfully restored %v runners in %v web rest dir", nums, m.RestDir)
}

func (m *Manager) Status() (rss map[string]RunnerStatus) {
	m.runnerLock.RLock()
	defer m.runnerLock.RUnlock()
	rss = make(map[string]RunnerStatus)
	for key, conf := range m.runnerConfigs {
		if r, ex := m.runners[key]; ex {
			rss[r.Name()] = r.Status()
			continue
		}
		rss[conf.RunnerName] = RunnerStatus{
			Name:           conf.RunnerName,
			ReaderStats:    StatsInfo{},
			ParserStats:    StatsInfo{},
			TransformStats: make(map[string]StatsInfo),
			SenderStats:    make(map[string]StatsInfo),
			RunningStatus:  RunnerStopped,
		}
	}
	return rss
}

func (m *Manager) Errors() (rss map[string]ErrorsResult) {
	m.runnerLock.RLock()
	defer m.runnerLock.RUnlock()
	rss = make(map[string]ErrorsResult)
	for key, conf := range m.runnerConfigs {
		if r, ex := m.runners[key]; ex {
			if runnerErr, ok := r.(RunnerErrors); ok {
				rss[r.Name()] = runnerErr.GetErrors()
				continue
			}
		}
		rss[conf.RunnerName] = ErrorsResult{}
	}
	return rss
}

func (m *Manager) Error(name string) (rss ErrorsResult, err error) {
	m.runnerLock.RLock()
	defer m.runnerLock.RUnlock()
	for key := range m.runnerConfigs {
		if r, ex := m.runners[key]; ex {
			if r.Name() != name {
				continue
			}

			if runnerErr, ok := r.(RunnerErrors); ok {
				return runnerErr.GetErrors(), nil
			}
			return rss, ErrNotSupport
		}
	}
	return rss, ErrNotExist
}

func (m *Manager) Configs() (rss map[string]RunnerConfig) {
	rss = make(map[string]RunnerConfig)
	tmpRss := make(map[string]RunnerConfig)
	m.runnerLock.RLock()
	for k, v := range m.runnerConfigs {
		if filepath.Dir(k) == m.RestDir {
			v.IsInWebFolder = true
		}
		tmpRss[k] = v
	}
	deepCopyByJSON(&rss, &tmpRss)
	m.runnerLock.RUnlock()
	return
}

func (m *Manager) getDeepCopyConfig(name string) (filename string, conf RunnerConfig, err error) {
	filename = filepath.Join(m.RestDir, name+".conf")
	m.runnerLock.RLock()
	defer m.runnerLock.RUnlock()
	if tmpConf, ok := m.runnerConfigs[filename]; !ok {
		err = fmt.Errorf("runner %v is not found", filename)
	} else {
		deepCopyByJSON(&conf, &tmpConf)
	}
	return
}

// TrimSecretInfo 将配置文件中的 token 等鉴权相关信息去掉
func TrimSecretInfo(conf RunnerConfig, trimSk bool) RunnerConfig {
	prefix := SchemaFreeTokensPrefix
	keyName := []string{
		prefix + "pipeline_get_repo_token",
		prefix + "pipeline_post_data_token",
		prefix + "pipeline_post_rawtext_data_token",
		prefix + "pipeline_create_repo_token",
		prefix + "pipeline_update_repo_token",
		prefix + "pipeline_get_workflow_token",
		prefix + "pipeline_stop_workflow_token",
		prefix + "pipeline_start_workflow_token",
		prefix + "pipeline_create_workflow_token",
		prefix + "pipeline_Get_workflow_status_token",
	}

	// logDB tokens
	prefix = LogDBTokensPrefix
	keyName = append(keyName, []string{
		prefix + "pipeline_get_repo_token",
		prefix + "pipeline_create_repo_token",
		prefix + "create_logdb_repo_token",
		prefix + "update_logdb_repo_token",
		prefix + "get_logdb_repo_token",
		prefix + "create_export_token",
		prefix + "update_export_token",
		prefix + "get_export_token",
		prefix + "list_export_token",
	}...)

	// tsDB tokens
	prefix = TsDBTokensPrefix
	keyName = append(keyName, []string{
		prefix + "pipeline_get_repo_token",
		prefix + "create_tsdb_repo_token",
		prefix + "list_export_token",
		prefix + "create_tsdb_series_token",
		prefix + "create_export_token",
		prefix + "update_export_token",
		prefix + "get_export_token",
	}...)

	// kodo tokens
	prefix = KodoTokensPrefix
	keyName = append(keyName, []string{
		prefix + "pipeline_get_repo_token",
		prefix + "create_export_token",
		prefix + "update_export_token",
		prefix + "get_export_token",
		prefix + "list_export_token",
	}...)

	if trimSk {
		// Pandora sk
		keyName = append(keyName, []string{
			"pandora_sk",
		}...)
	}

	for i, sc := range conf.SendersConfig {
		for _, k := range keyName {
			delete(sc, k)
		}
		conf.SendersConfig[i] = sc
	}
	return conf
}

func (m *Manager) backupRunnerConfig(filename string, rconf RunnerConfig) error {
	if m.ServerBackup {
		return nil
	}
	confBytes, err := jsoniter.MarshalIndent(rconf, "", "    ")
	if err != nil {
		return fmt.Errorf("runner config %v marshal failed, err is %v", rconf, err)
	}
	// 判断默认备份文件夹是否存在，不存在就尝试创建
	if _, err := os.Stat(m.RestDir); err != nil {
		if os.IsNotExist(err) {
			if err = os.Mkdir(m.RestDir, DefaultDirPerm); err != nil && !os.IsExist(err) {
				return fmt.Errorf("rest default dir not exists and make dir failed, err is %v", err)
			}
		}
	}
	return ioutil.WriteFile(filename, confBytes, 0644)
}

func (m *Manager) UpdateToken(tokens []AuthTokens) (err error) {
	m.runnerLock.Lock()
	defer m.runnerLock.Unlock()
	errMsg := make([]string, 0)
	for _, token := range tokens {
		runnerPath := token.RunnerName
		if runner, ok := m.runners[runnerPath]; ok {
			if r, ok := runner.(TokenRefreshable); ok {
				token.RunnerName = runner.Name()
				if subErr := r.TokenRefresh(token); subErr != nil {
					errMsg = append(errMsg, subErr.Error())
					continue
				}
			}
		}
		if c, ok := m.runnerConfigs[runnerPath]; ok {
			if len(c.SendersConfig) > token.SenderIndex {
				for k, t := range token.SenderTokens {
					c.SendersConfig[token.SenderIndex][k] = t
				}
			}
			m.runnerConfigs[runnerPath] = c
		}
	}
	if len(errMsg) != 0 {
		err = errors.New(strings.Join(errMsg, "\n"))
	}
	return
}

func (m *Manager) AddRunner(name string, conf RunnerConfig, createTime time.Time) (err error) {
	conf.RunnerName = name
	conf.CreateTime = createTime.Format(time.RFC3339Nano)
	filename := filepath.Join(m.RestDir, name+".conf")
	if m.IsRunning(filename) {
		return fmt.Errorf("file %v runner is running", name)
	}
	if err = m.ForkRunner(filename, conf, true); err != nil {
		return fmt.Errorf("forkRunner %v error %v", name, err)
	}
	if err = m.backupRunnerConfig(filename, conf); err != nil {
		// 回滚, 删除创建的 runner, 备份配置文件失败，所以此处不需要从磁盘删除配置文件
		if rollBackErr := m.Remove(filename); rollBackErr != nil {
			log.Errorf("runner <%v> backup RunnerConfig error and rollback error %v", name, rollBackErr)
		}
	}
	return
}

func (m *Manager) UpdateRunner(name string, conf RunnerConfig) (err error) {
	filename, oldConf, err := m.getDeepCopyConfig(name)
	if err != nil {
		return err
	}
	conf.RunnerName = name
	conf.CreateTime = time.Now().Format(time.RFC3339Nano)
	if m.IsRunning(filename) {
		if subErr := m.Remove(filename); subErr != nil {
			return fmt.Errorf("remove runner %v error %v", filename, subErr)
		}
	}
	if err = m.ForkRunner(filename, conf, true); err != nil {
		if subErr := m.ForkRunner(filename, oldConf, true); subErr != nil {
			log.Errorf("forkRunner error and rollback old runner error %v", subErr)
		}
		return fmt.Errorf("forkRunner %v error %v", filename, err)
	}
	if err = m.backupRunnerConfig(filename, conf); err != nil {
		// 备份配置失败，回滚
		if subErr := m.Remove(filename); subErr != nil {
			log.Errorf("runner %v update backup config error and rollback error %v", filename, subErr)
		}
		if subErr := m.ForkRunner(filename, oldConf, true); subErr != nil {
			log.Errorf("runner %v update backup config error and rollback error %v", filename, subErr)
		}
	}
	return
}

func (m *Manager) StartRunner(name string) (err error) {
	filename, conf, err := m.getDeepCopyConfig(name)
	if err != nil {
		return err
	}
	if conf.IsStopped == false {
		return fmt.Errorf("runner %v has already started", filename)
	}
	conf.IsStopped = false
	if err = m.ForkRunner(filename, conf, true); err != nil {
		return fmt.Errorf("forkRunner %v error %v", filename, err)
	}
	if err = m.backupRunnerConfig(filename, conf); err != nil {
		// 备份配置文件失败，回滚
		if subErr := m.RemoveWithConfig(filename, false); subErr != nil {
			log.Errorf("runner %v start backup config error and rollback error %v", name, subErr)
		} else {
			conf.IsStopped = true
			m.setRunnerConfig(filename, conf)
		}
	}
	return
}

func (m *Manager) setRunnerConfig(filename string, conf RunnerConfig) {
	m.runnerLock.Lock()
	m.runnerConfigs[filename] = conf
	m.runnerLock.Unlock()
}

func (m *Manager) StopRunner(name string) (err error) {
	filename, conf, err := m.getDeepCopyConfig(name)
	if err != nil {
		return err
	}
	if conf.IsStopped == true {
		return fmt.Errorf("runner %v has already stopped", filename)
	}
	conf.IsStopped = true
	if !m.IsRunning(filename) {
		m.setRunnerConfig(filename, conf)
		return
	}
	if err = m.RemoveWithConfig(filename, false); err != nil {
		return fmt.Errorf("remove runner %v error %v", filename, err)
	}
	m.setRunnerConfig(filename, conf)
	if err = m.backupRunnerConfig(filename, conf); err != nil {
		// 备份配置文件失败，回滚
		conf.IsStopped = false
		if subErr := m.ForkRunner(filename, conf, true); subErr != nil {
			log.Errorf("runner %v stop backup config error and rollback error %v", name, subErr)
		}
	}
	return
}

// ResetRunner 必须在runner实例存在下才可以reset, reset是调用runner本身的方法，
// 而runner stop实际上是销毁实例，所以先要启动runner
func (m *Manager) ResetRunner(name string) (err error) {
	filename, conf, err := m.getDeepCopyConfig(name)
	if err != nil {
		return err
	}
	status := conf.IsStopped
	if conf.IsStopped {
		conf.IsStopped = false
		//此处先启动runner
		if err = m.ForkRunner(filename, conf, true); err != nil {
			return fmt.Errorf("start %v for reset error %v, as runner is only resetable for alive", filename, err)
		}
	}
	r, runnerOk := m.readRunners(filename)
	if !runnerOk {
		return fmt.Errorf("runner %v is not found", filename)
	}

	runnerReset, ok := r.(Resetable)
	if !ok {
		//如果runner不支持reset函数，直接返回
		return fmt.Errorf("runner %v is not resetable runner", filename)
	}

	if subErr := m.Remove(filename); subErr != nil {
		log.Errorf("remove runner %v for reset error %v", filename, subErr)
	}
	conf.IsStopped = status
	// 出错的话，回滚并报错
	resetErr := runnerReset.Reset()
	if resetErr != nil {
		log.Errorf("reset runner %v error %v", filename, resetErr)
		// 此处就算失败也了不能直接return，需要回滚
	}
	conf.IsStopped = false
	err = m.ForkRunner(filename, conf, true)
	if err != nil {
		return fmt.Errorf("forkRunner %v for reset error %v, resetErr is %v", filename, err, resetErr)
	}
	if resetErr != nil {
		err = resetErr
	}
	return
}

func (m *Manager) readRunners(filename string) (Runner, bool) {
	m.runnerLock.RLock()
	defer m.runnerLock.RUnlock()
	r, ok := m.runners[filename]
	return r, ok
}

func (m *Manager) DeleteRunner(name string) (err error) {
	filename, conf, err := m.getDeepCopyConfig(name)
	if err != nil {
		return err
	}
	if conf.IsStopped {
		m.runnerLock.Lock()
		delete(m.runnerConfigs, filename)
		m.runnerLock.Unlock()
	}
	r, runnerOk := m.readRunners(filename)
	if runnerOk {
		if err = m.Remove(filename); err != nil {
			return fmt.Errorf("remove runner %v error %v", filename, err)
		}
		if runnerReset, ok := r.(Resetable); ok {
			runnerReset.Reset()
		}
	}
	if err = os.Remove(filename); err != nil {
		if os.IsNotExist(err) {
			err = nil
			return
		}
		// 回滚
		if subErr := m.ForkRunner(filename, conf, true); subErr != nil {
			log.Errorf("remove runner %v error and rollback error %v", filename, subErr)
		}
		return fmt.Errorf("remove runner %v error %v", filename, err)
	}
	return
}
