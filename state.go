package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

func emptyMonitorData() *MonitorData {
	return &MonitorData{MQTT: make(map[string]*MQTTMonitorData), Ping: make(map[string]*PingMonitorData), HTTP: make(map[string]*HTTPMonitorData), Exec: make(map[string]*ExecMonitorData)}
}

// Called with monitorData locked. Copies values so a failed transaction cannot
// modify any live monitor, override, or configuration.
func copyMonitorData(source *MonitorData) *MonitorData {
	state := emptyMonitorData()
	for k, v := range source.MQTT {
		entry := *v
		entry.History = append([]TimedEntry(nil), v.History...)
		entry.SampleDays = copyDayBuckets(v.SampleDays)
		state.MQTT[k] = &entry
	}
	for k, v := range source.Ping {
		entry := *v
		entry.OKDays = copyDayBuckets(v.OKDays)
		entry.ErrorDays = copyDayBuckets(v.ErrorDays)
		state.Ping[k] = &entry
	}
	for k, v := range source.HTTP {
		entry := *v
		entry.OKDays = copyDayBuckets(v.OKDays)
		entry.ErrorDays = copyDayBuckets(v.ErrorDays)
		state.HTTP[k] = &entry
	}
	for k, v := range source.Exec {
		entry := *v
		entry.OKDays = copyDayBuckets(v.OKDays)
		entry.ErrorDays = copyDayBuckets(v.ErrorDays)
		state.Exec[k] = &entry
	}
	return state
}

func copyWebEdits(source map[monitorKey]webEdit) map[monitorKey]webEdit {
	result := make(map[monitorKey]webEdit, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func publishMonitorData(state *MonitorData, edits map[monitorKey]webEdit) {
	monitorData.MQTT = state.MQTT
	monitorData.Ping = state.Ping
	monitorData.HTTP = state.HTTP
	monitorData.Exec = state.Exec
	webEdits = edits
}

func decodePersistence(store *sqlitePersistence) (*MonitorData, map[monitorKey]webEdit, []TimedEntry, error) {
	state := emptyMonitorData()
	edits := make(map[monitorKey]webEdit)
	for key, data := range store.rows {
		var value interface{}
		switch key.Kind {
		case "mqtt":
			entry := new(MQTTMonitorData)
			state.MQTT[key.Key] = entry
			value = entry
		case "ping":
			entry := new(PingMonitorData)
			state.Ping[key.Key] = entry
			value = entry
		case "http":
			entry := new(HTTPMonitorData)
			state.HTTP[key.Key] = entry
			value = entry
		case "exec":
			entry := new(ExecMonitorData)
			state.Exec[key.Key] = entry
			value = entry
		case "edit/mqtt", "edit/ping", "edit/http", "edit/exec":
			var edit webEdit
			if err := json.Unmarshal(data, &edit); err != nil {
				return nil, nil, nil, fmt.Errorf("decode %s/%s: %w", key.Kind, key.Key, err)
			}
			if err := validateWebEdit(edit); err != nil {
				return nil, nil, nil, err
			}
			edits[monitorKey{strings.TrimPrefix(key.Kind, "edit/"), key.Key}] = edit
			continue
		default:
			return nil, nil, nil, fmt.Errorf("unknown persisted monitor kind %q", key.Kind)
		}
		if err := json.Unmarshal(data, value); err != nil {
			return nil, nil, nil, fmt.Errorf("decode %s/%s: %w", key.Kind, key.Key, err)
		}
	}
	for key, entry := range state.MQTT {
		if !validTimeout(entry.CustomTimeout) {
			return nil, nil, nil, fmt.Errorf("invalid saved MQTT timeout for %q", key)
		}
		// Preserve deletions written by version 1 as explicit web edits.
		if entry.Deleted {
			edit := edits[monitorKey{"mqtt", key}]
			edit.Deleted = true
			edits[monitorKey{"mqtt", key}] = edit
		}
	}
	logs := make([]TimedEntry, len(store.logs))
	for i, entry := range store.logs {
		logs[i] = entry.Entry
	}
	return state, edits, logs, nil
}

func mqttTopicConfigured(c *Config, topic string) bool {
	if c.Monitor.MQTT.Server == "" {
		return false
	}
	for _, target := range c.Monitor.MQTT.Targets {
		if matchMQTTTopic(target.Topic, topic) {
			return true
		}
	}
	return false
}

func topicName(c *Config, topic string) string {
	for _, target := range c.Monitor.MQTT.Targets {
		if target.Topic == topic && target.Name != "" {
			return target.Name
		}
	}
	return topic
}

// Reconcile measured state with YAML, applying explicit web overrides only when
// requested. Edits for removed YAML targets are pruned along with their state.
func reconcileState(c *Config, state *MonitorData, edits map[monitorKey]webEdit, keepEdits bool) {
	configured := make(map[monitorKey]bool)
	for key, entry := range state.MQTT {
		if !mqttTopicConfigured(c, key) {
			delete(state.MQTT, key)
			continue
		}
		configured[monitorKey{"mqtt", key}] = true
		entry.Name = topicName(c, key)
		if !keepEdits {
			entry.CustomTimeout = 0
			entry.Deleted = false
		}
		if len(entry.History) > c.Monitor.MQTT.History {
			entry.History = entry.History[len(entry.History)-c.Monitor.MQTT.History:]
		}
		entry.AvgTransmit = mqttAverageTransmit(entry.History)
	}
	// MQTT deletion tombstones may outlive the last received message.
	for key := range edits {
		if key.Kind == "mqtt" && mqttTopicConfigured(c, key.Key) {
			configured[key] = true
		}
	}
	for _, target := range c.Monitor.Ping.Targets {
		key := monitorKey{"ping", target.Address}
		configured[key] = true
		entry := state.Ping[target.Address]
		if entry == nil {
			entry = new(PingMonitorData)
			state.Ping[target.Address] = entry
		}
		entry.Name = target.Name
		entry.Interval = inherit(target.Interval, c.Monitor.Ping.Interval)
		entry.Threshold = inherit(target.Threshold, c.Monitor.Ping.Threshold)
	}
	for key := range state.Ping {
		if !configured[monitorKey{"ping", key}] {
			delete(state.Ping, key)
		}
	}
	for _, target := range c.Monitor.HTTP.Targets {
		key := monitorKey{"http", target.Address}
		configured[key] = true
		entry := state.HTTP[target.Address]
		if entry == nil {
			entry = new(HTTPMonitorData)
			state.HTTP[target.Address] = entry
		}
		entry.Name = target.Name
		entry.Value = target.Value
		entry.Interval = inherit(target.Interval, c.Monitor.HTTP.Interval)
		entry.Timeout = inherit(target.Timeout, c.Monitor.HTTP.Timeout)
		entry.Threshold = inherit(target.Threshold, c.Monitor.HTTP.Threshold)
	}
	for key := range state.HTTP {
		if !configured[monitorKey{"http", key}] {
			delete(state.HTTP, key)
		}
	}
	for _, target := range c.Monitor.Exec.Targets {
		key := monitorKey{"exec", target.Command}
		configured[key] = true
		entry := state.Exec[target.Command]
		if entry == nil {
			entry = new(ExecMonitorData)
			state.Exec[target.Command] = entry
		}
		entry.Name = target.Name
		entry.Interval = inherit(target.Interval, c.Monitor.Exec.Interval)
		entry.Timeout = inherit(target.Timeout, c.Monitor.Exec.Timeout)
		entry.Threshold = inherit(target.Threshold, c.Monitor.Exec.Threshold)
	}
	for key := range state.Exec {
		if !configured[monitorKey{"exec", key}] {
			delete(state.Exec, key)
		}
	}
	for key, edit := range edits {
		if !configured[key] || !keepEdits {
			delete(edits, key)
			continue
		}
		applyWebEdit(state, key, edit)
	}
}

func inherit(value, fallback int) int {
	if value != 0 {
		return value
	}
	return fallback
}

func applyWebEdit(state *MonitorData, key monitorKey, edit webEdit) {
	switch key.Kind {
	case "mqtt":
		if entry := state.MQTT[key.Key]; entry != nil {
			entry.Deleted = edit.Deleted
			if edit.CustomTimeout != nil {
				entry.CustomTimeout = *edit.CustomTimeout
			}
		}
	case "ping":
		if edit.Deleted {
			delete(state.Ping, key.Key)
		} else if entry := state.Ping[key.Key]; entry != nil {
			if edit.Interval != nil {
				entry.Interval = *edit.Interval
			}
			if edit.Threshold != nil {
				entry.Threshold = *edit.Threshold
			}
		}
	case "http":
		if edit.Deleted {
			delete(state.HTTP, key.Key)
		} else if entry := state.HTTP[key.Key]; entry != nil {
			if edit.Interval != nil {
				entry.Interval = *edit.Interval
			}
			if edit.Timeout != nil {
				entry.Timeout = *edit.Timeout
			}
			if edit.Threshold != nil {
				entry.Threshold = *edit.Threshold
			}
		}
	case "exec":
		if edit.Deleted {
			delete(state.Exec, key.Key)
		} else if entry := state.Exec[key.Key]; entry != nil {
			if edit.Interval != nil {
				entry.Interval = *edit.Interval
			}
			if edit.Timeout != nil {
				entry.Timeout = *edit.Timeout
			}
			if edit.Threshold != nil {
				entry.Threshold = *edit.Threshold
			}
		}
	}
}

func validateWebEdit(edit webEdit) error {
	for name, field := range map[string]*int{"interval": edit.Interval, "timeout": edit.Timeout, "threshold": edit.Threshold} {
		if field != nil && !validMonitorInteger(name, *field) {
			return fmt.Errorf("invalid saved %s", name)
		}
	}
	if edit.CustomTimeout != nil && !validTimeout(*edit.CustomTimeout) {
		return fmt.Errorf("invalid saved MQTT timeout")
	}
	return nil
}

func validMonitorInteger(name string, value int) bool {
	if value < 0 {
		return false
	}
	switch name {
	case "interval":
		return uint64(value) <= uint64(math.MaxInt64/int64(time.Second))
	case "timeout":
		return uint64(value) <= uint64(math.MaxInt64/int64(time.Millisecond))
	}
	return true
}

func validateConfig(c *Config) error {
	if c.LogSize < 1 || c.Monitor.MQTT.History < 1 {
		return fmt.Errorf("logsize and MQTT history must be positive")
	}
	if !validTimeout(c.Monitor.MQTT.StandardTimeout) || c.Monitor.MQTT.StandardTimeout == 0 {
		return fmt.Errorf("MQTT standardtimeout must be finite and positive")
	}
	validate := func(interval, timeout, threshold int) error {
		if !validMonitorInteger("interval", interval) || !validMonitorInteger("timeout", timeout) || threshold < 0 {
			return fmt.Errorf("monitor interval, timeout, and threshold must be nonnegative and fit their supported ranges")
		}
		return nil
	}
	for _, v := range [][3]int{{c.Monitor.Ping.Interval, 0, c.Monitor.Ping.Threshold}, {c.Monitor.HTTP.Interval, c.Monitor.HTTP.Timeout, c.Monitor.HTTP.Threshold}, {c.Monitor.Exec.Interval, c.Monitor.Exec.Timeout, c.Monitor.Exec.Threshold}} {
		if err := validate(v[0], v[1], v[2]); err != nil {
			return err
		}
	}
	for _, v := range c.Monitor.Ping.Targets {
		if err := validate(v.Interval, 0, v.Threshold); err != nil {
			return err
		}
	}
	for _, v := range c.Monitor.HTTP.Targets {
		if err := validate(v.Interval, v.Timeout, v.Threshold); err != nil {
			return err
		}
	}
	for _, v := range c.Monitor.Exec.Targets {
		if err := validate(v.Interval, v.Timeout, v.Threshold); err != nil {
			return err
		}
	}
	for _, v := range c.Monitor.MQTT.Targets {
		if v.Timeout < 0 {
			return fmt.Errorf("MQTT timeout must be nonnegative")
		}
	}
	return nil
}
