package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const persistenceFixture = `
logsize: 2
monitor:
  mqtt:
    server: broker
    history: 2
    targets:
      - topic: home/#
        timeout: 60
      - topic: home/temp
        name: Temperature
  ping:
    targets:
      - address: router
        name: Router
        interval: 60
      - address: backup
        name: Backup
  http:
    targets:
      - address: http://service
        name: Service
  exec:
    targets:
      - command: "true"
        name: Command
`

func persistenceConfig(t *testing.T, path string) *Config {
	t.Helper()
	c := new(Config)
	if err := yaml.Unmarshal([]byte(persistenceFixture), c); err != nil {
		t.Fatal(err)
	}
	setDefaults(c)
	c.Persistence.DB = path
	return c
}

func startTestPersistence(t *testing.T, path string) {
	t.Helper()
	if err := configurePersistenceForConfig(persistenceConfig(t, path), true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closePersistence(); err != nil {
			t.Error(err)
		}
	})
}

func postWebItem(t *testing.T, handler http.HandlerFunc, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler(w, req)
	return w
}

func totalDatabaseChanges(t *testing.T, store *sqlitePersistence) int {
	t.Helper()
	var total int
	if err := store.db.QueryRow("SELECT total_changes()").Scan(&total); err != nil {
		t.Fatal(err)
	}
	return total
}

type persistenceMessage struct{ topic, payload string }

func (m persistenceMessage) Duplicate() bool   { return false }
func (m persistenceMessage) Qos() byte         { return 0 }
func (m persistenceMessage) Retained() bool    { return false }
func (m persistenceMessage) Topic() string     { return m.topic }
func (m persistenceMessage) MessageID() uint16 { return 0 }
func (m persistenceMessage) Payload() []byte   { return []byte(m.payload) }
func (m persistenceMessage) Ack()              {}

func TestPersistenceWritesOnlyChanges(t *testing.T) {
	resetGlobals()
	startTestPersistence(t, filepath.Join(t.TempDir(), "state.db"))
	store := persistenceStore
	before := totalDatabaseChanges(t, store)
	for i := 0; i < 3; i++ {
		evaluateMQTTState()
		if err := savePersistence(); err != nil {
			t.Fatal(err)
		}
	}
	if got := totalDatabaseChanges(t, store); got != before {
		t.Fatalf("idle saves wrote %d rows", got-before)
	}
	monitorData.Lock()
	monitorData.Ping["router"].TotalOK++
	monitorData.Unlock()
	if err := savePersistence(); err != nil {
		t.Fatal(err)
	}
	if got := totalDatabaseChanges(t, store); got != before+1 {
		t.Fatalf("one changed monitor wrote %d rows", got-before)
	}
	for i, want := range []int{1, 1, 2} {
		before = totalDatabaseChanges(t, store)
		log(fmt.Sprintf("entry-%d", i))
		if got := totalDatabaseChanges(t, store) - before; got != want {
			t.Fatalf("adding log %d changed %d rows, want %d", i, got, want)
		}
	}
	before = totalDatabaseChanges(t, store)
	if err := savePersistence(); err != nil {
		t.Fatal(err)
	}
	if totalDatabaseChanges(t, store) != before {
		t.Fatal("unchanged logs were rewritten")
	}
}

func TestPersistenceWebEditsSurviveRestartAndResetOnReload(t *testing.T) {
	resetGlobals()
	path := filepath.Join(t.TempDir(), "state.db")
	startTestPersistence(t, path)
	onMessageReceived(nil, persistenceMessage{"home/temp", "23"})
	forms := []url.Values{
		{"type": {"ping"}, "name": {"router"}, "interval": {"15"}, "threshold": {"4"}},
		{"type": {"http"}, "name": {"http://service"}, "interval": {"20"}, "timeout": {"750"}, "threshold": {"5"}},
		{"type": {"exec"}, "name": {"true"}, "interval": {"25"}, "timeout": {"900"}, "threshold": {"6"}},
		{"type": {"mqtt"}, "name": {"home/temp"}, "timeout": {"120.5"}},
	}
	for _, form := range forms {
		if w := postWebItem(t, configWebItem, form); w.Code != http.StatusSeeOther {
			t.Fatalf("edit failed: %s", w.Body.String())
		}
	}
	if w := postWebItem(t, deleteWebItem, url.Values{"type": {"ping"}, "name": {"backup"}}); w.Code != http.StatusSeeOther {
		t.Fatal(w.Body.String())
	}
	resetGlobals()
	startTestPersistence(t, path)
	if e := monitorData.Ping["router"]; e.Interval != 15 || e.Threshold != 4 {
		t.Fatalf("lost ping edit: %+v", e)
	}
	if e := monitorData.HTTP["http://service"]; e.Interval != 20 || e.Timeout != 750 || e.Threshold != 5 {
		t.Fatalf("lost HTTP edit: %+v", e)
	}
	if e := monitorData.Exec["true"]; e.Interval != 25 || e.Timeout != 900 || e.Threshold != 6 {
		t.Fatalf("lost Exec edit: %+v", e)
	}
	if e := monitorData.MQTT["home/temp"]; e.CustomTimeout != 120.5 || e.Samples != 1 {
		t.Fatalf("lost MQTT edit/data: %+v", e)
	}
	if monitorData.Ping["backup"] != nil {
		t.Fatal("deleted target reappeared on restart")
	}
	if err := configurePersistenceForConfig(persistenceConfig(t, path), false); err != nil {
		t.Fatal(err)
	}
	if monitorData.Ping["router"].Interval != 60 || monitorData.HTTP["http://service"].Timeout != 5000 || monitorData.Exec["true"].Threshold != 2 || monitorData.MQTT["home/temp"].CustomTimeout != 0 {
		t.Fatal("YAML did not replace web settings on reload")
	}
	if monitorData.Ping["backup"] == nil || len(webEdits) != 0 {
		t.Fatal("reload did not restore deletion and clear overrides")
	}
	resetGlobals()
	startTestPersistence(t, path)
	if monitorData.Ping["router"].Interval != 60 || monitorData.Ping["backup"] == nil {
		t.Fatal("cleared overrides returned after another restart")
	}
}

func TestPersistenceDeletesAllMonitorTypes(t *testing.T) {
	resetGlobals()
	path := filepath.Join(t.TempDir(), "state.db")
	startTestPersistence(t, path)
	onMessageReceived(nil, persistenceMessage{"home/temp", "23"})
	for _, key := range []monitorKey{{"mqtt", "home/temp"}, {"ping", "router"}, {"http", "http://service"}, {"exec", "true"}} {
		if w := postWebItem(t, deleteWebItem, url.Values{"type": {key.Kind}, "name": {key.Key}}); w.Code != http.StatusSeeOther {
			t.Fatal(w.Body.String())
		}
	}
	resetGlobals()
	startTestPersistence(t, path)
	if !monitorData.MQTT["home/temp"].Deleted || monitorData.Ping["router"] != nil || monitorData.HTTP["http://service"] != nil || monitorData.Exec["true"] != nil {
		t.Fatal("deletions did not survive restart")
	}
	onMessageReceived(nil, persistenceMessage{"home/temp", "24"})
	if monitorData.MQTT["home/temp"].Samples != 1 {
		t.Fatal("deleted MQTT topic accepted a message")
	}
	if err := configurePersistenceForConfig(persistenceConfig(t, path), false); err != nil {
		t.Fatal(err)
	}
	if monitorData.MQTT["home/temp"].Deleted || monitorData.Ping["router"] == nil || monitorData.HTTP["http://service"] == nil || monitorData.Exec["true"] == nil {
		t.Fatal("reload did not restore YAML targets")
	}
}

func TestPersistenceResetUsesTargetYAMLValue(t *testing.T) {
	resetGlobals()
	path := filepath.Join(t.TempDir(), "state.db")
	c := persistenceConfig(t, path)
	c.Monitor.Ping.Targets[0].Interval = 90
	if err := configurePersistenceForConfig(c, true); err != nil {
		t.Fatal(err)
	}
	defer closePersistence()
	for _, value := range []string{"15", "0"} {
		if w := postWebItem(t, configWebItem, url.Values{"type": {"ping"}, "name": {"router"}, "interval": {value}}); w.Code != http.StatusSeeOther {
			t.Fatal(w.Body.String())
		}
	}
	if monitorData.Ping["router"].Interval != 90 {
		t.Fatal("zero did not restore target's YAML interval")
	}
	if _, ok := webEdits[monitorKey{"ping", "router"}]; ok {
		t.Fatal("reset left an override")
	}
}

func TestPersistenceFiltersTopicsAndUsesCurrentNames(t *testing.T) {
	resetGlobals()
	path := filepath.Join(t.TempDir(), "state.db")
	startTestPersistence(t, path)
	onMessageReceived(nil, persistenceMessage{"home/temp", "23"})
	onMessageReceived(nil, persistenceMessage{"home/other", "24"})
	resetGlobals()
	c := persistenceConfig(t, path)
	c.Monitor.MQTT.Targets = c.Monitor.MQTT.Targets[1:]
	c.Monitor.MQTT.Targets[0].Name = "Renamed"
	if err := configurePersistenceForConfig(c, true); err != nil {
		t.Fatal(err)
	}
	if monitorData.MQTT["home/temp"].Name != "Renamed" || monitorData.MQTT["home/other"] != nil {
		t.Fatal("MQTT membership or names ignored current YAML")
	}
	onMessageReceived(nil, persistenceMessage{"home/other", "25"})
	if monitorData.MQTT["home/other"] != nil {
		t.Fatal("old subscription resurrected removed topic")
	}
	c.Monitor.MQTT.Server = ""
	if err := configurePersistenceForConfig(c, false); err != nil {
		t.Fatal(err)
	}
	if len(monitorData.MQTT) != 0 {
		t.Fatal("disabled MQTT restored monitors")
	}
}

func TestPersistenceSwitchKeepsDatabasesSeparate(t *testing.T) {
	resetGlobals()
	oldPath := filepath.Join(t.TempDir(), "old.db")
	nextPath := filepath.Join(t.TempDir(), "next.db")
	startTestPersistence(t, oldPath)
	monitorData.Lock()
	monitorData.Ping["router"].TotalOK = 5
	monitorData.Unlock()
	if w := postWebItem(t, configWebItem, url.Values{"type": {"ping"}, "name": {"router"}, "interval": {"15"}}); w.Code != http.StatusSeeOther {
		t.Fatal(w.Body.String())
	}
	next, err := openSQLitePersistence(nextPath)
	if err != nil {
		t.Fatal(err)
	}
	state := copyMonitorData(&monitorData)
	state.Ping["router"].TotalOK = 99
	interval := 10
	edits := map[monitorKey]webEdit{{"ping", "router"}: {Interval: &interval}}
	applyWebEdit(state, monitorKey{"ping", "router"}, edits[monitorKey{"ping", "router"}])
	snap, err := snapshotState(persistenceConfig(t, nextPath), state, edits, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := next.writeSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	next.db.Close()
	if err := configurePersistenceForConfig(persistenceConfig(t, nextPath), false); err != nil {
		t.Fatal(err)
	}
	if e := monitorData.Ping["router"]; e.TotalOK != 99 || e.Interval != 10 {
		t.Fatalf("selected database was not loaded: %+v", e)
	}
	old, err := openSQLitePersistence(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	defer old.db.Close()
	saved, savedEdits, _, err := decodePersistence(old)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Ping["router"].TotalOK != 5 || *savedEdits[monitorKey{"ping", "router"}].Interval != 15 {
		t.Fatal("old database was contaminated by selected database")
	}
}

func TestPersistenceRejectsFailedReloadAtomically(t *testing.T) {
	resetGlobals()
	path := filepath.Join(t.TempDir(), "state.db")
	configFile = filepath.Join(t.TempDir(), "config.yml")
	writeConfig := func(path string) {
		t.Helper()
		raw := fmt.Sprintf("hostname: original\npersistence:\n  db: %s\nmonitor:\n  ping:\n    targets:\n      - address: router\n", path)
		if err := os.WriteFile(configFile, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig(path)
	if !loadConfig() {
		t.Fatal("initial load failed")
	}
	defer closePersistence()
	oldConfig, oldStore, oldEntry := config, persistenceStore, monitorData.Ping["router"]
	writeConfig(t.TempDir())
	if loadConfig() {
		t.Fatal("invalid database path accepted")
	}
	if config != oldConfig || persistenceStore != oldStore || monitorData.Ping["router"] != oldEntry {
		t.Fatal("failed reload changed live state")
	}
	badPath := filepath.Join(t.TempDir(), "corrupt.db")
	bad, err := openSQLitePersistence(badPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.db.Exec("INSERT INTO monitor_state VALUES ('ping','router', ?, '')", []byte("{broken")); err != nil {
		t.Fatal(err)
	}
	bad.db.Close()
	writeConfig(badPath)
	if loadConfig() {
		t.Fatal("malformed saved state accepted")
	}
	if config != oldConfig || persistenceStore != oldStore || monitorData.Ping["router"] != oldEntry {
		t.Fatal("partial restore leaked into live state")
	}
}

// A missing config file is only tolerated on the initial load. On reload it must
// be fatal, otherwise treating it as an empty config would delete live monitor
// state and persist those deletions to disk.
func TestReloadMissingConfigPreservesState(t *testing.T) {
	resetGlobals()
	path := filepath.Join(t.TempDir(), "state.db")
	configFile = filepath.Join(t.TempDir(), "config.yml")
	raw := fmt.Sprintf("persistence:\n  db: %s\nmonitor:\n  ping:\n    targets:\n      - address: router\n        name: Router\n", path)
	if err := os.WriteFile(configFile, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if !loadConfig() {
		t.Fatal("initial load failed")
	}
	defer closePersistence()
	oldConfig, oldStore, oldEntry := config, persistenceStore, monitorData.Ping["router"]
	if oldEntry == nil {
		t.Fatal("ping target was not configured")
	}
	if err := os.Remove(configFile); err != nil {
		t.Fatal(err)
	}
	if loadConfig() {
		t.Fatal("reload with a missing config file was accepted")
	}
	if config != oldConfig || persistenceStore != oldStore || monitorData.Ping["router"] != oldEntry {
		t.Fatal("reload with a missing config file changed live state")
	}
	// The persisted row must survive too: the destructive variant deletes it.
	var data []byte
	if err := oldStore.db.QueryRow("SELECT data FROM monitor_state WHERE kind='ping' AND key='router'").Scan(&data); err != nil {
		t.Fatalf("persisted ping state disappeared after failed reload: %v", err)
	}
}

func TestPersistenceFailedWebSaveRollsBack(t *testing.T) {
	resetGlobals()
	startTestPersistence(t, filepath.Join(t.TempDir(), "state.db"))
	store := persistenceStore
	if _, err := store.db.Exec(`CREATE TRIGGER reject_write BEFORE INSERT ON monitor_state BEGIN SELECT RAISE(ABORT, 'simulated write failure'); END`); err != nil {
		t.Fatal(err)
	}
	defer store.db.Exec("DROP TRIGGER reject_write")
	w := postWebItem(t, configWebItem, url.Values{"type": {"ping"}, "name": {"router"}, "interval": {"15"}})
	if w.Code != http.StatusInternalServerError || monitorData.Ping["router"].Interval != 60 || len(webEdits) != 0 {
		t.Fatal("failed edit was acknowledged or applied")
	}
	w = postWebItem(t, deleteWebItem, url.Values{"type": {"ping"}, "name": {"router"}})
	if w.Code != http.StatusInternalServerError || monitorData.Ping["router"] == nil {
		t.Fatal("failed deletion was acknowledged or applied")
	}
}

func TestPersistenceRejectsInvalidInputs(t *testing.T) {
	resetGlobals()
	startTestPersistence(t, filepath.Join(t.TempDir(), "state.db"))
	onMessageReceived(nil, persistenceMessage{"home/temp", "23"})
	before := totalDatabaseChanges(t, persistenceStore)
	for _, value := range []string{"NaN", "Inf", "-Inf", "-1", "invalid"} {
		if w := postWebItem(t, configWebItem, url.Values{"type": {"mqtt"}, "name": {"home/temp"}, "timeout": {value}}); w.Code != http.StatusBadRequest {
			t.Fatalf("accepted timeout %q", value)
		}
	}
	for _, value := range []string{"-1", "9223372036854775807", "invalid"} {
		if w := postWebItem(t, configWebItem, url.Values{"type": {"ping"}, "name": {"router"}, "interval": {value}}); w.Code != http.StatusBadRequest {
			t.Fatalf("accepted interval %q", value)
		}
	}
	if totalDatabaseChanges(t, persistenceStore) != before {
		t.Fatal("invalid edits modified the database")
	}
	if err := savePersistence(); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"logsize: -1", "monitor:\n  mqtt:\n    history: -1", "monitor:\n  mqtt:\n    standardtimeout: .nan"} {
		c := new(Config)
		if err := yaml.Unmarshal([]byte(field), c); err != nil {
			t.Fatal(err)
		}
		setDefaults(c)
		if err := configurePersistenceForConfig(c, false); err == nil {
			t.Fatalf("accepted invalid config %s", field)
		}
	}
}

func TestPersistenceConcurrentMessagesReloadAndEdits(t *testing.T) {
	resetGlobals()
	path := filepath.Join(t.TempDir(), "state.db")
	startTestPersistence(t, path)
	var workers sync.WaitGroup
	errs := make(chan error, 60)
	workers.Add(3)
	go func() {
		defer workers.Done()
		for i := 0; i < 30; i++ {
			onMessageReceived(nil, persistenceMessage{"home/temp", fmt.Sprint(i)})
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 30; i++ {
			if err := configurePersistenceForConfig(persistenceConfig(t, path), false); err != nil {
				errs <- err
			}
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 30; i++ {
			w := postWebItem(t, configWebItem, url.Values{"type": {"ping"}, "name": {"router"}, "interval": {"15"}})
			if w.Code != http.StatusSeeOther {
				errs <- fmt.Errorf("edit: %s", w.Body.String())
			}
		}
	}()
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent persistence deadlocked")
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if monitorData.MQTT["home/temp"].Samples != 30 {
		t.Fatal("reload lost MQTT samples")
	}
}

func TestPersistenceAbruptExit(t *testing.T) {
	if path := os.Getenv("JANITOR_PERSISTENCE_CRASH_DB"); path != "" {
		resetGlobals()
		if err := configurePersistenceForConfig(persistenceConfig(t, path), true); err != nil {
			t.Fatal(err)
		}
		onMessageReceived(nil, persistenceMessage{"home/temp", "23"})
		if w := postWebItem(t, configWebItem, url.Values{"type": {"ping"}, "name": {"router"}, "interval": {"15"}}); w.Code != http.StatusSeeOther {
			t.Fatal(w.Body.String())
		}
		log("committed-before-exit")
		os.Exit(0) // Deliberately bypass shutdown, DB close, and WAL checkpoint.
	}
	resetGlobals()
	path := filepath.Join(t.TempDir(), "state.db")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestPersistenceAbruptExit$")
	cmd.Env = append(os.Environ(), "JANITOR_PERSISTENCE_CRASH_DB="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper: %s: %v", out, err)
	}
	startTestPersistence(t, path)
	if monitorData.MQTT["home/temp"].Samples != 1 || monitorData.Ping["router"].Interval != 15 || len(logHistory) != 1 || logHistory[0].Value != "committed-before-exit" {
		t.Fatal("completed events lost after abrupt exit")
	}
}

func TestPersistenceMigrationAndPathResolution(t *testing.T) {
	resetGlobals()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := openSQLitePersistence(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := store.db.Exec("UPDATE schema_version SET version=1"); err != nil {
		t.Fatal(err)
	}
	for i, value := range []string{"latest", "older"} {
		if _, err := store.db.Exec("INSERT INTO log_history VALUES (?,?,?)", i, now.Add(-time.Duration(i)*time.Second).Format(time.RFC3339Nano), value); err != nil {
			t.Fatal(err)
		}
	}
	store.db.Close()
	startTestPersistence(t, path)
	if len(logHistory) != 2 || logHistory[0].Value != "latest" || logHistory[1].Value != "older" {
		t.Fatal("migration reordered history")
	}
	var version int
	persistenceStore.db.QueryRow("SELECT version FROM schema_version").Scan(&version)
	if version != persistenceSchemaVersion {
		t.Fatal("schema not upgraded")
	}
	resolved, err := resolvePersistencePath("~")
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if resolved != home {
		t.Fatalf("~ resolved to %q, want %q", resolved, home)
	}
}

func TestPersistenceMonitoringStopsBeforeClose(t *testing.T) {
	resetGlobals()
	started := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { once.Do(func() { close(started) }); <-r.Context().Done() }))
	defer server.Close()
	c := persistenceConfig(t, filepath.Join(t.TempDir(), "state.db"))
	c.Monitor.MQTT.Server = ""
	c.Monitor.Ping.Targets = nil
	c.Monitor.Exec.Targets = nil
	c.Monitor.HTTP.Targets[0].Address = server.URL
	c.Monitor.HTTP.Timeout = 10000
	if err := configurePersistenceForConfig(c, true); err != nil {
		t.Fatal(err)
	}
	stop := monitoringLoop()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		stop()
		t.Fatal("HTTP check not started")
	}
	stop()
	if monitorData.HTTP[server.URL].TotalError != 0 {
		t.Fatal("shutdown cancellation recorded a monitor failure")
	}
	if err := closePersistence(); err != nil {
		t.Fatal(err)
	}
	monitoringContext = context.Background()
}

func TestPersistenceLoadConfigRestartAndReloadPrecedence(t *testing.T) {
	resetGlobals()
	path := filepath.Join(t.TempDir(), "state.db")
	configFile = filepath.Join(t.TempDir(), "config.yml")
	raw := fmt.Sprintf("persistence:\n  db: %s\nmonitor:\n  ping:\n    targets:\n      - address: router\n        interval: 90\n", path)
	if err := os.WriteFile(configFile, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if !loadConfig() {
		t.Fatal("startup failed")
	}
	if w := postWebItem(t, configWebItem, url.Values{"type": {"ping"}, "name": {"router"}, "interval": {"15"}}); w.Code != http.StatusSeeOther {
		t.Fatal(w.Body.String())
	}
	resetGlobals()
	if !loadConfig() {
		t.Fatal("restart failed")
	}
	defer closePersistence()
	if monitorData.Ping["router"].Interval != 15 {
		t.Fatal("startup discarded saved edit")
	}
	if !loadConfig() {
		t.Fatal("reload failed")
	}
	if monitorData.Ping["router"].Interval != 90 {
		t.Fatal("reload did not restore YAML interval")
	}
}

func TestPersistenceDatabaseAliasIsAReload(t *testing.T) {
	resetGlobals()
	path := filepath.Join(t.TempDir(), "state.db")
	startTestPersistence(t, path)
	alias := filepath.Join(t.TempDir(), "alias.db")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	if w := postWebItem(t, configWebItem, url.Values{"type": {"ping"}, "name": {"router"}, "interval": {"15"}}); w.Code != http.StatusSeeOther {
		t.Fatal(w.Body.String())
	}
	old := persistenceStore
	if err := configurePersistenceForConfig(persistenceConfig(t, alias), false); err != nil {
		t.Fatal(err)
	}
	if persistenceStore != old || monitorData.Ping["router"].Interval != 60 {
		t.Fatal("alias to current database was treated as a switch")
	}
}

func TestPersistenceDiscardsHTTPResultFromPreviousConfiguration(t *testing.T) {
	resetGlobals()
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release; fmt.Fprint(w, "ok") }))
	defer server.Close()
	c := persistenceConfig(t, filepath.Join(t.TempDir(), "old.db"))
	c.Monitor.MQTT.Server = ""
	c.Monitor.HTTP.Targets[0].Address = server.URL
	c.Monitor.HTTP.Targets[0].Value = "old expectation"
	if err := configurePersistenceForConfig(c, true); err != nil {
		t.Fatal(err)
	}
	defer closePersistence()
	done := make(chan struct{})
	go func() { checkHTTP(); close(done) }()
	<-started
	next := persistenceConfig(t, filepath.Join(t.TempDir(), "next.db"))
	next.Monitor.MQTT.Server = ""
	next.Monitor.HTTP.Targets[0].Address = server.URL
	next.Monitor.HTTP.Targets[0].Value = "ok"
	if err := configurePersistenceForConfig(next, false); err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	close(release)
	<-done
	entry := monitorData.HTTP[server.URL]
	if entry.TotalOK != 0 || entry.TotalError != 0 {
		t.Fatalf("old check changed replacement state: %+v", entry)
	}
}

func TestExecTimeoutDoesNotWaitForChildOutputPipe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix shell child process")
	}
	resetGlobals()
	started := time.Now()
	if performExecCheck("sleep 2; printf done", 20) {
		t.Fatal("timed out command succeeded")
	}
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("timeout waited for child output pipe: %s", elapsed)
	}
}

func TestPersistenceLiteralFilename(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not permit question marks in filenames")
	}
	resetGlobals()
	path := filepath.Join(t.TempDir(), "state?#% .db")
	startTestPersistence(t, path)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() == 0 {
		t.Fatal("SQLite used a different filename")
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("database permissions were not restricted")
	}
	resetGlobals()
	startTestPersistence(t, path)
	if monitorData.Ping["router"] == nil {
		t.Fatal("database did not reopen at literal filename")
	}
}
