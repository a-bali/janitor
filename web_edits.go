package main

import (
	"fmt"
	"net/http"
	"strconv"
)

func hasMonitor(state *MonitorData, key monitorKey) bool {
	switch key.Kind {
	case "mqtt":
		return state.MQTT[key.Key] != nil
	case "ping":
		return state.Ping[key.Key] != nil
	case "http":
		return state.HTTP[key.Key] != nil
	case "exec":
		return state.Exec[key.Key] != nil
	}
	return false
}

// A web edit is acknowledged only after its transaction commits. A failed save
// leaves live state untouched and the client receives an error instead of a redirect.
func commitWebEdit(key monitorKey, change func(*Config, *MonitorData, webEdit) (webEdit, error)) error {
	persistenceMu.Lock()
	defer persistenceMu.Unlock()
	monitorData.Lock()
	defer monitorData.Unlock()
	logLock.RLock()
	defer logLock.RUnlock()
	if !hasMonitor(&monitorData, key) {
		return fmt.Errorf("monitor no longer exists")
	}
	c := getConfig()
	state := copyMonitorData(&monitorData)
	edits := copyWebEdits(webEdits)
	edit, err := change(c, state, edits[key])
	if err != nil {
		return err
	}
	if edit.Deleted || edit.Interval != nil || edit.Timeout != nil || edit.Threshold != nil || edit.CustomTimeout != nil {
		edits[key] = edit
	} else {
		delete(edits, key)
	}
	applyWebEdit(state, key, edit)
	if persistenceStore != nil {
		snapshot, err := snapshotState(c, state, edits, logHistory)
		if err != nil {
			return err
		}
		if err := persistenceStore.writeSnapshot(snapshot); err != nil {
			return err
		}
	}
	publishMonitorData(state, edits)
	return nil
}

func webItemKey(w http.ResponseWriter, r *http.Request) (monitorKey, bool) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid form", http.StatusBadRequest)
		return monitorKey{}, false
	}
	key := monitorKey{r.Form.Get("type"), r.Form.Get("name")}
	if key.Key == "" || (key.Kind != "mqtt" && key.Kind != "ping" && key.Kind != "http" && key.Kind != "exec") {
		http.Error(w, "Unknown monitor", http.StatusBadRequest)
		return key, false
	}
	return key, true
}

// Deletes remain suppressed across restarts until Reload config reapplies YAML.
func deleteWebItem(w http.ResponseWriter, r *http.Request) {
	key, ok := webItemKey(w, r)
	if !ok {
		return
	}
	configurationMu.Lock()
	defer configurationMu.Unlock()
	if stopping.Load() {
		http.Error(w, "Application is shutting down", http.StatusServiceUnavailable)
		return
	}
	err := commitWebEdit(key, func(_ *Config, _ *MonitorData, edit webEdit) (webEdit, error) { edit.Deleted = true; return edit, nil })
	if err != nil {
		http.Error(w, "Unable to save deletion: "+err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Zero removes the corresponding web override and restores the YAML value.
func configWebItem(w http.ResponseWriter, r *http.Request) {
	key, ok := webItemKey(w, r)
	if !ok {
		return
	}
	values := make(map[string]int)
	var mqttTimeout *float64
	fields := []string{"interval", "threshold"}
	if key.Kind == "mqtt" {
		fields = nil
	} else if key.Kind != "ping" {
		fields = append(fields, "timeout")
	}
	for _, name := range fields {
		if raw, exists := r.Form[name]; exists {
			if len(raw) != 1 {
				http.Error(w, "Invalid "+name, http.StatusBadRequest)
				return
			}
			value, err := strconv.Atoi(raw[0])
			if err != nil || !validMonitorInteger(name, value) {
				http.Error(w, "Invalid "+name, http.StatusBadRequest)
				return
			}
			values[name] = value
		}
	}
	if key.Kind == "mqtt" {
		if raw, exists := r.Form["timeout"]; exists {
			if len(raw) != 1 {
				http.Error(w, "Invalid timeout", http.StatusBadRequest)
				return
			}
			value, err := strconv.ParseFloat(raw[0], 64)
			if err != nil || !validTimeout(value) {
				http.Error(w, "Timeout must be finite and nonnegative", http.StatusBadRequest)
				return
			}
			mqttTimeout = &value
		}
	}
	configurationMu.Lock()
	defer configurationMu.Unlock()
	if stopping.Load() {
		http.Error(w, "Application is shutting down", http.StatusServiceUnavailable)
		return
	}
	err := commitWebEdit(key, func(c *Config, state *MonitorData, edit webEdit) (webEdit, error) {
		if edit.Deleted {
			return edit, fmt.Errorf("monitor was deleted; reload configuration to restore it")
		}
		for name, value := range values {
			var field *int
			if value != 0 {
				v := value
				field = &v
			}
			switch name {
			case "interval":
				edit.Interval = field
			case "timeout":
				edit.Timeout = field
			case "threshold":
				edit.Threshold = field
			}
			if value == 0 {
				setMonitorInteger(state, key, name, yamlMonitorInteger(c, key, name))
			}
		}
		if mqttTimeout != nil {
			if *mqttTimeout == 0 {
				edit.CustomTimeout = nil
				state.MQTT[key.Key].CustomTimeout = 0
			} else {
				edit.CustomTimeout = mqttTimeout
			}
		}
		return edit, nil
	})
	if err != nil {
		http.Error(w, "Unable to save settings: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if key.Kind == "mqtt" {
		evaluateMQTTState()
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func yamlMonitorInteger(c *Config, key monitorKey, name string) int {
	interval, timeout, threshold := 0, 0, 0
	switch key.Kind {
	case "ping":
		interval, threshold = c.Monitor.Ping.Interval, c.Monitor.Ping.Threshold
		for _, target := range c.Monitor.Ping.Targets {
			if target.Address == key.Key {
				interval = inherit(target.Interval, interval)
				threshold = inherit(target.Threshold, threshold)
				break
			}
		}
	case "http":
		interval, timeout, threshold = c.Monitor.HTTP.Interval, c.Monitor.HTTP.Timeout, c.Monitor.HTTP.Threshold
		for _, target := range c.Monitor.HTTP.Targets {
			if target.Address == key.Key {
				interval = inherit(target.Interval, interval)
				timeout = inherit(target.Timeout, timeout)
				threshold = inherit(target.Threshold, threshold)
				break
			}
		}
	case "exec":
		interval, timeout, threshold = c.Monitor.Exec.Interval, c.Monitor.Exec.Timeout, c.Monitor.Exec.Threshold
		for _, target := range c.Monitor.Exec.Targets {
			if target.Command == key.Key {
				interval = inherit(target.Interval, interval)
				timeout = inherit(target.Timeout, timeout)
				threshold = inherit(target.Threshold, threshold)
				break
			}
		}
	}
	switch name {
	case "interval":
		return interval
	case "timeout":
		return timeout
	default:
		return threshold
	}
}

func setMonitorInteger(state *MonitorData, key monitorKey, name string, value int) {
	switch key.Kind {
	case "ping":
		entry := state.Ping[key.Key]
		switch name {
		case "interval":
			entry.Interval = value
		case "threshold":
			entry.Threshold = value
		}
	case "http":
		entry := state.HTTP[key.Key]
		switch name {
		case "interval":
			entry.Interval = value
		case "timeout":
			entry.Timeout = value
		case "threshold":
			entry.Threshold = value
		}
	case "exec":
		entry := state.Exec[key.Key]
		switch name {
		case "interval":
			entry.Interval = value
		case "timeout":
			entry.Timeout = value
		case "threshold":
			entry.Threshold = value
		}
	}
}
