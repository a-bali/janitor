package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// ---- helpers ----------------------------------------------------------------

func resetGlobals() {
	configLock.Lock()
	config = &Config{}
	setDefaults(config)
	configLock.Unlock()

	logLock.Lock()
	logHistory = nil
	logLock.Unlock()

	monitorData.Lock()
	monitorData.MQTT = make(map[string]*MQTTMonitorData)
	monitorData.Ping = make(map[string]*PingMonitorData)
	monitorData.HTTP = make(map[string]*HTTPMonitorData)
	monitorData.Exec = make(map[string]*ExecMonitorData)
	monitorData.Unlock()
}

// ---- matchMQTTTopic ---------------------------------------------------------

func TestMatchMQTTTopic(t *testing.T) {
	tests := []struct {
		pattern string
		subject string
		want    bool
	}{
		// exact match
		{"home/sensor/temp", "home/sensor/temp", true},
		{"home/sensor/temp", "home/sensor/humidity", false},
		// single-level wildcard
		{"home/+/temp", "home/sensor/temp", true},
		{"home/+/temp", "home/sensor/humidity", false},
		{"home/+/temp", "home/a/b/temp", false},
		// multi-level wildcard
		{"home/#", "home/sensor/temp", true},
		{"home/#", "home/a/b/c/d", true},
		{"home/#", "away/sensor", false},
		// wildcard as sole segment
		{"#", "anything/here", true},
		{"#", "single", true},
		// wildcard deduplication scenario: broad includes specific
		{"sensors/#", "sensors/temp", true},
		{"sensors/#", "sensors/a/b", true},
		// no match on different root
		{"a/b/c", "x/b/c", false},
		// pattern longer than subject (was index-out-of-range panic)
		{"a/b/c", "a/b", false},
		{"a/b/c/d", "a/b", false},
		// wildcard characters must occupy a complete topic level
		{"home/+suffix", "home/sensor", false},
		{"home/pre+fix", "home/pre+fix", false},
		{"home/#suffix", "home/sensor", false},
	}
	for _, tt := range tests {
		got := matchMQTTTopic(tt.pattern, tt.subject)
		if got != tt.want {
			t.Errorf("matchMQTTTopic(%q, %q) = %v; want %v", tt.pattern, tt.subject, got, tt.want)
		}
	}
}

// ---- relaTime ---------------------------------------------------------------

func TestRelaTime_Zero(t *testing.T) {
	if got := relaTime(time.Time{}); got != "inf" {
		t.Errorf("relaTime(zero) = %q; want %q", got, "inf")
	}
}

func TestRelaTime_RecentSeconds(t *testing.T) {
	// A time 5 seconds ago should produce a sub-minute string.
	past := time.Now().Add(-5 * time.Second)
	got := relaTime(past)
	if strings.Contains(got, "m") || strings.Contains(got, "d") {
		t.Errorf("relaTime(5s ago) = %q; expected seconds-only string", got)
	}
}

func TestRelaTime_Minutes(t *testing.T) {
	past := time.Now().Add(-90 * time.Second)
	got := relaTime(past)
	if !strings.Contains(got, "m") {
		t.Errorf("relaTime(90s ago) = %q; expected minute component", got)
	}
}

func TestRelaTime_Days(t *testing.T) {
	past := time.Now().Add(-25 * time.Hour)
	got := relaTime(past)
	if !strings.HasPrefix(got, "1d") {
		t.Errorf("relaTime(25h ago) = %q; expected prefix '1d'", got)
	}
}

// ---- setDefaults ------------------------------------------------------------

func TestSetDefaults_AllZero(t *testing.T) {
	c := &Config{}
	setDefaults(c)

	if c.LogSize != MAXLOGSIZE {
		t.Errorf("LogSize = %d; want %d", c.LogSize, MAXLOGSIZE)
	}
	if c.Web.Port != 8080 {
		t.Errorf("Web.Port = %d; want 8080", c.Web.Port)
	}
	if c.Monitor.MQTT.History != 10 {
		t.Errorf("Monitor.MQTT.History = %d; want 10", c.Monitor.MQTT.History)
	}
	if c.Monitor.MQTT.Port != 1883 {
		t.Errorf("Monitor.MQTT.Port = %d; want 1883", c.Monitor.MQTT.Port)
	}
	if c.Monitor.MQTT.StandardTimeout != 1.5 {
		t.Errorf("Monitor.MQTT.StandardTimeout = %v; want 1.5", c.Monitor.MQTT.StandardTimeout)
	}
	if c.Monitor.Ping.Interval != 60 {
		t.Errorf("Ping.Interval = %d; want 60", c.Monitor.Ping.Interval)
	}
	if c.Monitor.Ping.Threshold != 2 {
		t.Errorf("Ping.Threshold = %d; want 2", c.Monitor.Ping.Threshold)
	}
	if c.Monitor.HTTP.Interval != 60 {
		t.Errorf("HTTP.Interval = %d; want 60", c.Monitor.HTTP.Interval)
	}
	if c.Monitor.HTTP.Timeout != 5000 {
		t.Errorf("HTTP.Timeout = %d; want 5000", c.Monitor.HTTP.Timeout)
	}
	if c.Monitor.HTTP.Threshold != 2 {
		t.Errorf("HTTP.Threshold = %d; want 2", c.Monitor.HTTP.Threshold)
	}
	if c.Monitor.Exec.Interval != 60 {
		t.Errorf("Exec.Interval = %d; want 60", c.Monitor.Exec.Interval)
	}
	if c.Monitor.Exec.Timeout != 5000 {
		t.Errorf("Exec.Timeout = %d; want 5000", c.Monitor.Exec.Timeout)
	}
	if c.Monitor.Exec.Threshold != 2 {
		t.Errorf("Exec.Threshold = %d; want 2", c.Monitor.Exec.Threshold)
	}
	if c.Alert.MQTT.Port != 1883 {
		t.Errorf("Alert.MQTT.Port = %d; want 1883", c.Alert.MQTT.Port)
	}
	if c.HostName == "" {
		t.Errorf("HostName should not be empty after setDefaults")
	}
}

func TestSetDefaults_PreservesExistingValues(t *testing.T) {
	c := &Config{}
	c.Web.Port = 9090
	c.LogSize = 500
	c.Monitor.Ping.Interval = 30
	setDefaults(c)

	if c.Web.Port != 9090 {
		t.Errorf("Web.Port overwritten; got %d want 9090", c.Web.Port)
	}
	if c.LogSize != 500 {
		t.Errorf("LogSize overwritten; got %d want 500", c.LogSize)
	}
	if c.Monitor.Ping.Interval != 30 {
		t.Errorf("Ping.Interval overwritten; got %d want 30", c.Monitor.Ping.Interval)
	}
}

// ---- YAML config parsing ----------------------------------------------------

func TestYAMLConfigParsing_Basic(t *testing.T) {
	raw := `
debug: true
logsize: 200
web:
  port: 9000
monitor:
  ping:
    interval: 30
    threshold: 3
    targets:
      - name: "Router"
        address: "192.168.1.1"
`
	c := new(Config)
	if err := yaml.Unmarshal([]byte(raw), c); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v", err)
	}
	setDefaults(c)

	if !c.Debug {
		t.Error("Debug should be true")
	}
	if c.LogSize != 200 {
		t.Errorf("LogSize = %d; want 200", c.LogSize)
	}
	if c.Web.Port != 9000 {
		t.Errorf("Web.Port = %d; want 9000", c.Web.Port)
	}
	if c.Monitor.Ping.Interval != 30 {
		t.Errorf("Ping.Interval = %d; want 30", c.Monitor.Ping.Interval)
	}
	if c.Monitor.Ping.Threshold != 3 {
		t.Errorf("Ping.Threshold = %d; want 3", c.Monitor.Ping.Threshold)
	}
	if len(c.Monitor.Ping.Targets) != 1 {
		t.Fatalf("Expected 1 ping target, got %d", len(c.Monitor.Ping.Targets))
	}
	if c.Monitor.Ping.Targets[0].Name != "Router" {
		t.Errorf("Ping target name = %q; want 'Router'", c.Monitor.Ping.Targets[0].Name)
	}
	if c.Monitor.Ping.Targets[0].Address != "192.168.1.1" {
		t.Errorf("Ping target address = %q; want '192.168.1.1'", c.Monitor.Ping.Targets[0].Address)
	}
}

func TestYAMLConfigParsing_AlertChannels(t *testing.T) {
	raw := `
alert:
  telegram:
    token: "abc123"
    chat: 9876543210
  gotify:
    token: "gotify-token"
    server: "http://gotify.local"
  mqtt:
    server: "mqtt.local"
    port: 1883
    topic: "alerts/janitor"
`
	c := new(Config)
	if err := yaml.Unmarshal([]byte(raw), c); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v", err)
	}
	if c.Alert.Telegram.Token != "abc123" {
		t.Errorf("Telegram.Token = %q", c.Alert.Telegram.Token)
	}
	if c.Alert.Telegram.Chat != 9876543210 {
		t.Errorf("Telegram.Chat = %d", c.Alert.Telegram.Chat)
	}
	if c.Alert.Gotify.Server != "http://gotify.local" {
		t.Errorf("Gotify.Server = %q", c.Alert.Gotify.Server)
	}
	if c.Alert.MQTT.Topic != "alerts/janitor" {
		t.Errorf("Alert.MQTT.Topic = %q", c.Alert.MQTT.Topic)
	}
}

func TestYAMLConfigParsing_MQTTTargets(t *testing.T) {
	raw := `
monitor:
  mqtt:
    server: "mqtt.local"
    port: 1884
    history: 20
    standardtimeout: 2.0
    targets:
      - topic: "sensors/#"
        name: "All sensors"
      - topic: "home/temp"
        name: "Temperature"
        timeout: 300
`
	c := new(Config)
	if err := yaml.Unmarshal([]byte(raw), c); err != nil {
		t.Fatalf("yaml.Unmarshal failed: %v", err)
	}
	if c.Monitor.MQTT.Server != "mqtt.local" {
		t.Errorf("MQTT.Server = %q", c.Monitor.MQTT.Server)
	}
	if c.Monitor.MQTT.History != 20 {
		t.Errorf("MQTT.History = %d; want 20", c.Monitor.MQTT.History)
	}
	if len(c.Monitor.MQTT.Targets) != 2 {
		t.Fatalf("Expected 2 MQTT targets, got %d", len(c.Monitor.MQTT.Targets))
	}
	if c.Monitor.MQTT.Targets[1].Timeout != 300 {
		t.Errorf("MQTT target[1].Timeout = %d; want 300", c.Monitor.MQTT.Targets[1].Timeout)
	}
}

func TestLoadConfigRefreshesInheritedTargetValues(t *testing.T) {
	resetGlobals()
	dir := t.TempDir()
	configFile = dir + "/config.yml"
	raw := `
monitor:
  ping:
    interval: 15
    threshold: 4
    targets:
      - name: "Router"
        address: "router.local"
`
	if err := os.WriteFile(configFile, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}

	monitorData.Lock()
	monitorData.Ping["router.local"] = &PingMonitorData{
		Name: "Router", Interval: 99, Threshold: 99,
	}
	monitorData.Unlock()

	if !loadConfig() {
		t.Fatal("loadConfig returned false")
	}

	monitorData.RLock()
	entry := monitorData.Ping["router.local"]
	monitorData.RUnlock()
	if entry.Interval != 15 || entry.Threshold != 4 {
		t.Fatalf("inherited values = interval %d, threshold %d; want 15, 4", entry.Interval, entry.Threshold)
	}
}

// ---- calcStats --------------------------------------------------------------

func TestCalcStats_Empty(t *testing.T) {
	resetGlobals()
	up, down := calcStats()
	for _, k := range []string{"mqtt", "ping", "http", "exec"} {
		if up[k] != 0 || down[k] != 0 {
			t.Errorf("calcStats empty: %s up=%d down=%d; want 0/0", k, up[k], down[k])
		}
	}
}

func TestCalcStats_MixedStatuses(t *testing.T) {
	resetGlobals()

	monitorData.Lock()
	monitorData.Ping["host1"] = &PingMonitorData{Status: STATUS_OK}
	monitorData.Ping["host2"] = &PingMonitorData{Status: STATUS_ERROR}
	monitorData.Ping["host3"] = &PingMonitorData{Status: STATUS_WARN}
	monitorData.HTTP["url1"] = &HTTPMonitorData{Status: STATUS_ERROR}
	monitorData.HTTP["url2"] = &HTTPMonitorData{Status: STATUS_OK}
	monitorData.Exec["cmd1"] = &ExecMonitorData{Status: STATUS_OK}
	monitorData.MQTT["topic1"] = &MQTTMonitorData{Status: STATUS_ERROR}
	monitorData.MQTT["topic2"] = &MQTTMonitorData{Status: STATUS_OK, Deleted: false}
	monitorData.MQTT["topic3"] = &MQTTMonitorData{Status: STATUS_ERROR, Deleted: true}
	monitorData.Unlock()

	up, down := calcStats()

	// Ping: 2 up (OK+WARN), 1 down
	if up["ping"] != 2 {
		t.Errorf("ping up = %d; want 2", up["ping"])
	}
	if down["ping"] != 1 {
		t.Errorf("ping down = %d; want 1", down["ping"])
	}
	// HTTP: 1 up, 1 down
	if up["http"] != 1 {
		t.Errorf("http up = %d; want 1", up["http"])
	}
	if down["http"] != 1 {
		t.Errorf("http down = %d; want 1", down["http"])
	}
	// Exec: 1 up, 0 down
	if up["exec"] != 1 {
		t.Errorf("exec up = %d; want 1", up["exec"])
	}
	if down["exec"] != 0 {
		t.Errorf("exec down = %d; want 0", down["exec"])
	}
	// MQTT: topic3 is deleted so not counted; 1 up, 1 down
	if up["mqtt"] != 1 {
		t.Errorf("mqtt up = %d; want 1", up["mqtt"])
	}
	if down["mqtt"] != 1 {
		t.Errorf("mqtt down = %d; want 1", down["mqtt"])
	}
}

// ---- API handlers -----------------------------------------------------------

func TestServeAPIStats(t *testing.T) {
	resetGlobals()

	monitorData.Lock()
	monitorData.Ping["h1"] = &PingMonitorData{Status: STATUS_OK}
	monitorData.Ping["h2"] = &PingMonitorData{Status: STATUS_ERROR}
	monitorData.HTTP["u1"] = &HTTPMonitorData{Status: STATUS_OK}
	monitorData.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/stats", nil)
	w := httptest.NewRecorder()
	serveAPIStats(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type = %q; want application/json", ct)
	}

	var stats StatsData
	if err := json.NewDecoder(resp.Body).Decode(&stats); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// 2 OK (h1, u1), 1 ERROR (h2)
	if stats.OkCount != 2 {
		t.Errorf("ok = %d; want 2", stats.OkCount)
	}
	if stats.ErrCount != 1 {
		t.Errorf("error = %d; want 1", stats.ErrCount)
	}
}

func TestServeAPIData(t *testing.T) {
	resetGlobals()

	monitorData.Lock()
	monitorData.Ping["myhost"] = &PingMonitorData{Name: "MyHost", Status: STATUS_OK}
	monitorData.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/data", nil)
	w := httptest.NewRecorder()
	serveAPIData(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}

	var data map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("decode: %v", err)
	}
	pingData, ok := data["Ping"].(map[string]interface{})
	if !ok {
		t.Fatal("Ping key missing from /api/data response")
	}
	if _, exists := pingData["myhost"]; !exists {
		t.Error("expected 'myhost' in Ping data")
	}
}

func TestServeAPIMetrics(t *testing.T) {
	resetGlobals()

	configLock.Lock()
	config.HostName = "testhost"
	configLock.Unlock()

	monitorData.Lock()
	monitorData.Ping["h1"] = &PingMonitorData{Status: STATUS_OK}
	monitorData.HTTP["u1"] = &HTTPMonitorData{Status: STATUS_ERROR}
	monitorData.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	w := httptest.NewRecorder()
	serveAPIMetrics(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d; want 200", resp.StatusCode)
	}

	body := w.Body.String()
	if !strings.Contains(body, "janitor_targets") {
		t.Error("metrics body missing 'janitor_targets'")
	}
	if !strings.Contains(body, "testhost") {
		t.Error("metrics body missing hostname 'testhost'")
	}
	if !strings.Contains(body, `state="up"`) {
		t.Error("metrics body missing state=up")
	}
	if !strings.Contains(body, `state="down"`) {
		t.Error("metrics body missing state=down")
	}
}

func TestServeAPIMetricsEscapesHostname(t *testing.T) {
	resetGlobals()
	configLock.Lock()
	config.HostName = `host"with\newline` + "\n"
	configLock.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/api/metrics", nil)
	w := httptest.NewRecorder()
	serveAPIMetrics(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `host="host\"with\newline\n"`) {
		t.Fatalf("hostname was not Prometheus-escaped: %q", body)
	}
}

// ---- performHTTPCheck -------------------------------------------------------

func TestPerformHTTPCheck_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "hello world")
	}))
	defer srv.Close()

	ok, errStr, val := performHTTPCheck(srv.URL, "", 5000)
	if !ok {
		t.Errorf("expected ok=true, got errStr=%q", errStr)
	}
	if val != "hello world" {
		t.Errorf("val = %q; want 'hello world'", val)
	}
	if errStr != "" {
		t.Errorf("errStr = %q; want empty", errStr)
	}
}

func TestPerformHTTPCheck_PatternMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "status: running")
	}))
	defer srv.Close()

	ok, _, _ := performHTTPCheck(srv.URL, "running", 5000)
	if !ok {
		t.Error("expected ok=true for matching pattern")
	}
}

func TestPerformHTTPCheck_PatternNoMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "status: stopped")
	}))
	defer srv.Close()

	ok, errStr, _ := performHTTPCheck(srv.URL, "running", 5000)
	if ok {
		t.Error("expected ok=false when pattern not found")
	}
	if errStr == "" {
		t.Error("expected non-empty errStr when pattern not found")
	}
}

func TestPerformHTTPCheck_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	ok, errStr, _ := performHTTPCheck(srv.URL, "", 5000)
	if ok {
		t.Error("expected ok=false for 404 response")
	}
	if !strings.Contains(errStr, "404") {
		t.Errorf("errStr = %q; want to contain '404'", errStr)
	}
}

func TestPerformHTTPCheck_ConnectionRefused(t *testing.T) {
	ok, errStr, _ := performHTTPCheck("http://127.0.0.1:1", "", 1000)
	if ok {
		t.Error("expected ok=false for refused connection")
	}
	if errStr == "" {
		t.Error("expected non-empty errStr for connection refused")
	}
}

func TestPerformHTTPCheck_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		fmt.Fprint(w, "late")
	}))
	defer srv.Close()

	ok, errStr, _ := performHTTPCheck(srv.URL, "", 50)
	if ok {
		t.Error("expected ok=false for timed-out request")
	}
	if errStr == "" {
		t.Error("expected non-empty errStr on timeout")
	}
}

func TestPerformHTTPCheck_RejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, maxHTTPResponseSize+1))
	}))
	defer srv.Close()

	ok, errValue, value := performHTTPCheck(srv.URL, "", 5000)
	if ok {
		t.Fatal("expected oversized response to fail")
	}
	if !strings.Contains(errValue, "exceeds") {
		t.Fatalf("error = %q; want oversized-response error", errValue)
	}
	if value != "" {
		t.Fatalf("value length = %d; want empty value", len(value))
	}
}

// ---- performExecCheck -------------------------------------------------------

func TestPerformExecCheck_Success(t *testing.T) {
	ok := performExecCheck("true", 5000)
	if !ok {
		t.Error("expected ok=true for 'true' command")
	}
}

func TestPerformExecCheck_Failure(t *testing.T) {
	ok := performExecCheck("false", 5000)
	if ok {
		t.Error("expected ok=false for 'false' command")
	}
}

func TestPerformExecCheck_Timeout(t *testing.T) {
	ok := performExecCheck("sleep 10", 50)
	if ok {
		t.Error("expected ok=false when command times out")
	}
}

func TestPerformExecCheck_InvalidCommand(t *testing.T) {
	ok := performExecCheck("this_command_does_not_exist_xyz123", 5000)
	if ok {
		t.Error("expected ok=false for non-existent command")
	}
}

// ---- deleteWebItem handler --------------------------------------------------

func TestDeleteWebItem_Ping(t *testing.T) {
	resetGlobals()

	configLock.Lock()
	config.Monitor.Ping.Targets = append(config.Monitor.Ping.Targets, struct {
		Name      string
		Address   string
		Interval  int
		Threshold int
	}{Name: "TestHost", Address: "1.2.3.4"})
	configLock.Unlock()

	monitorData.Lock()
	monitorData.Ping["1.2.3.4"] = &PingMonitorData{Name: "TestHost"}
	monitorData.Unlock()

	form := url.Values{"type": {"ping"}, "name": {"1.2.3.4"}}
	req := httptest.NewRequest(http.MethodPost, "/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	deleteWebItem(w, req)

	monitorData.RLock()
	_, exists := monitorData.Ping["1.2.3.4"]
	monitorData.RUnlock()
	if exists {
		t.Error("expected ping entry to be deleted from monitorData")
	}

	configLock.RLock()
	found := false
	for _, tgt := range config.Monitor.Ping.Targets {
		if tgt.Address == "1.2.3.4" {
			found = true
		}
	}
	configLock.RUnlock()
	if found {
		t.Error("expected ping target to be removed from config")
	}
}

func TestDeleteWebItem_MQTT_SetsDeletedFlag(t *testing.T) {
	resetGlobals()

	monitorData.Lock()
	monitorData.MQTT["home/temp"] = &MQTTMonitorData{Name: "Temperature"}
	monitorData.Unlock()

	form := url.Values{"type": {"mqtt"}, "name": {"home/temp"}}
	req := httptest.NewRequest(http.MethodPost, "/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	deleteWebItem(w, req)

	monitorData.RLock()
	entry, exists := monitorData.MQTT["home/temp"]
	monitorData.RUnlock()
	if !exists {
		t.Fatal("MQTT entry should remain (with Deleted flag) not be fully removed")
	}
	if !entry.Deleted {
		t.Error("expected MQTT entry Deleted flag to be true")
	}
}

func TestDeleteWebItem_HTTP(t *testing.T) {
	resetGlobals()

	configLock.Lock()
	config.Monitor.HTTP.Targets = append(config.Monitor.HTTP.Targets, struct {
		Name      string
		Address   string
		Value     string
		Interval  int
		Timeout   int
		Threshold int
	}{Name: "Test URL", Address: "http://test.local"})
	configLock.Unlock()

	monitorData.Lock()
	monitorData.HTTP["http://test.local"] = &HTTPMonitorData{Name: "Test URL"}
	monitorData.Unlock()

	form := url.Values{"type": {"http"}, "name": {"http://test.local"}}
	req := httptest.NewRequest(http.MethodPost, "/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	deleteWebItem(w, req)

	monitorData.RLock()
	_, exists := monitorData.HTTP["http://test.local"]
	monitorData.RUnlock()
	if exists {
		t.Error("expected HTTP entry to be deleted")
	}
}

// ---- configWebItem handler --------------------------------------------------

func TestConfigWebItem_PingInterval(t *testing.T) {
	resetGlobals()

	monitorData.Lock()
	monitorData.Ping["1.2.3.4"] = &PingMonitorData{Name: "host", Interval: 60}
	monitorData.Unlock()

	form := url.Values{"type": {"ping"}, "name": {"1.2.3.4"}, "interval": {"30"}}
	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	configWebItem(w, req)

	monitorData.RLock()
	got := monitorData.Ping["1.2.3.4"].Interval
	monitorData.RUnlock()
	if got != 30 {
		t.Errorf("Interval = %d; want 30", got)
	}
}

func TestConfigWebItem_HTTPThreshold(t *testing.T) {
	resetGlobals()

	monitorData.Lock()
	monitorData.HTTP["http://srv"] = &HTTPMonitorData{Name: "srv", Threshold: 2}
	monitorData.Unlock()

	form := url.Values{"type": {"http"}, "name": {"http://srv"}, "threshold": {"5"}}
	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	configWebItem(w, req)

	monitorData.RLock()
	got := monitorData.HTTP["http://srv"].Threshold
	monitorData.RUnlock()
	if got != 5 {
		t.Errorf("Threshold = %d; want 5", got)
	}
}

func TestConfigWebItem_MQTTCustomTimeout(t *testing.T) {
	resetGlobals()

	monitorData.Lock()
	monitorData.MQTT["home/temp"] = &MQTTMonitorData{Name: "Temperature"}
	monitorData.Unlock()

	form := url.Values{"type": {"mqtt"}, "name": {"home/temp"}, "timeout": {"120.5"}}
	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	configWebItem(w, req)

	monitorData.RLock()
	got := monitorData.MQTT["home/temp"].CustomTimeout
	monitorData.RUnlock()
	if got != 120.5 {
		t.Errorf("CustomTimeout = %v; want 120.5", got)
	}
}

func TestConfigWebItem_InvalidValue(t *testing.T) {
	resetGlobals()

	monitorData.Lock()
	monitorData.Ping["1.2.3.4"] = &PingMonitorData{Name: "host", Interval: 60}
	monitorData.Unlock()

	form := url.Values{"type": {"ping"}, "name": {"1.2.3.4"}, "interval": {"notanumber"}}
	req := httptest.NewRequest(http.MethodPost, "/config", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	configWebItem(w, req) // should not panic

	monitorData.RLock()
	got := monitorData.Ping["1.2.3.4"].Interval
	monitorData.RUnlock()
	if got != 60 {
		t.Errorf("Interval should remain 60 after invalid input, got %d", got)
	}
}

// ---- log and logHistory -----------------------------------------------------

func TestLog_PrependsEntry(t *testing.T) {
	resetGlobals()

	log("first message")
	log("second message")

	logLock.RLock()
	defer logLock.RUnlock()

	if len(logHistory) != 2 {
		t.Fatalf("logHistory len = %d; want 2", len(logHistory))
	}
	// Most recent first
	if logHistory[0].Value != "second message" {
		t.Errorf("logHistory[0] = %q; want 'second message'", logHistory[0].Value)
	}
	if logHistory[1].Value != "first message" {
		t.Errorf("logHistory[1] = %q; want 'first message'", logHistory[1].Value)
	}
}

func TestLog_TruncatesAtLogSize(t *testing.T) {
	resetGlobals()

	configLock.Lock()
	config.LogSize = 3
	configLock.Unlock()

	for i := 0; i < 5; i++ {
		log(fmt.Sprintf("msg%d", i))
	}

	logLock.RLock()
	n := len(logHistory)
	logLock.RUnlock()

	if n != 3 {
		t.Errorf("logHistory len = %d; want 3 (truncated to LogSize)", n)
	}
}

// ---- concurrent access safety -----------------------------------------------

func TestConcurrentCalcStats(t *testing.T) {
	resetGlobals()

	monitorData.Lock()
	for i := 0; i < 20; i++ {
		monitorData.Ping[fmt.Sprintf("host%d", i)] = &PingMonitorData{Status: STATUS_OK}
	}
	monitorData.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			calcStats()
		}()
	}
	wg.Wait()
}
