package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const persistenceSchemaVersion = 2

// persistenceMu serializes saving, database replacement, and configuration
// publication. Callers must release monitorData and logLock before acquiring it.
var persistenceMu sync.Mutex

type monitorKey struct{ Kind, Key string }
type persistedMonitorRow struct {
	Kind, Key string
	Data      []byte
}
type persistedLog struct {
	Sequence int64
	Entry    TimedEntry
}
type persistenceSnapshot struct {
	Rows []persistedMonitorRow
	Logs []TimedEntry
}

type sqlitePersistence struct {
	db       *sql.DB
	path     string
	mu       sync.Mutex
	rows     map[monitorKey][]byte
	logs     []persistedLog
	sequence int64
}

// Web edits are separate from measured state so YAML remains authoritative for
// names and target membership without discarding explicit overrides.
type webEdit struct {
	Interval      *int     `json:",omitempty"`
	Timeout       *int     `json:",omitempty"`
	Threshold     *int     `json:",omitempty"`
	CustomTimeout *float64 `json:",omitempty"`
	Deleted       bool     `json:",omitempty"`
}

// Protected by monitorData's mutex.
var webEdits = make(map[monitorKey]webEdit)

func resolvePersistencePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if value == "~" {
			value = home
		} else {
			value = filepath.Join(home, value[2:])
		}
	}
	return filepath.Abs(value)
}

func openSQLitePersistence(path string) (*sqlitePersistence, error) {
	path, err := resolvePersistencePath(path)
	if err != nil || path == "" {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	// Do not chmod an existing directory: it may be shared or be the user's home.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("create database file: %w", err)
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return nil, fmt.Errorf("set database permissions: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	// Encode filesystem names as URI paths so ?, #, and % stay literal.
	uriPath := filepath.ToSlash(path)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	dsn := (&url.URL{Scheme: "file", Path: uriPath}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(err error) (*sqlitePersistence, error) { db.Close(); return nil, err }
	for _, statement := range []string{"PRAGMA busy_timeout = 5000", "PRAGMA journal_mode = WAL", "PRAGMA synchronous = FULL"} {
		if _, err := db.Exec(statement); err != nil {
			return fail(fmt.Errorf("configure database: %w", err))
		}
	}
	if err := migratePersistence(db); err != nil {
		return fail(err)
	}
	store := &sqlitePersistence{db: db, path: path, rows: make(map[monitorKey][]byte)}
	rows, err := db.Query("SELECT kind, key, data FROM monitor_state")
	if err != nil {
		return fail(err)
	}
	for rows.Next() {
		var key monitorKey
		var data []byte
		if err := rows.Scan(&key.Kind, &key.Key, &data); err != nil {
			rows.Close()
			return fail(err)
		}
		store.rows[key] = data
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fail(err)
	}
	logs, err := db.Query("SELECT sequence, timestamp, value FROM log_history ORDER BY sequence DESC")
	if err != nil {
		return fail(err)
	}
	for logs.Next() {
		var entry persistedLog
		var timestamp string
		if err := logs.Scan(&entry.Sequence, &timestamp, &entry.Entry.Value); err != nil {
			logs.Close()
			return fail(err)
		}
		entry.Entry.Timestamp, err = time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			logs.Close()
			return fail(err)
		}
		store.logs = append(store.logs, entry)
		if entry.Sequence > store.sequence {
			store.sequence = entry.Sequence
		}
	}
	err = logs.Err()
	logs.Close()
	if err != nil {
		return fail(err)
	}
	return store, nil
}

func migratePersistence(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)"); err != nil {
		return err
	}
	var version int
	err = tx.QueryRow("SELECT version FROM schema_version LIMIT 1").Scan(&version)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil && version != 1 && version != persistenceSchemaVersion {
		return fmt.Errorf("unsupported persistence schema version %d", version)
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS monitor_state (kind TEXT NOT NULL, key TEXT NOT NULL, data BLOB NOT NULL, updated_at TEXT NOT NULL, PRIMARY KEY (kind,key))`,
		`CREATE TABLE IF NOT EXISTS log_history (sequence INTEGER PRIMARY KEY, timestamp TEXT NOT NULL, value TEXT NOT NULL)`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	if version == 1 {
		// Version 1 numbered the newest log zero; version 2 allocates increasing IDs.
		if _, err := tx.Exec("UPDATE log_history SET sequence = -sequence"); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE schema_version SET version = ?", persistenceSchemaVersion); err != nil {
			return err
		}
	} else if version == 0 {
		if _, err := tx.Exec("INSERT INTO schema_version VALUES (?)", persistenceSchemaVersion); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Saves complete events synchronously. There is no timer, queued notification,
// or intentional durability window after this function returns successfully.
func savePersistence() error {
	persistenceMu.Lock()
	defer persistenceMu.Unlock()
	if persistenceStore == nil {
		return nil
	}
	return persistenceStore.flush()
}

func persistState() {
	if err := savePersistence(); err != nil {
		fmt.Printf("[persistence] unable to save state: %s\n", err)
	}
}

func closePersistence() error {
	persistenceMu.Lock()
	defer persistenceMu.Unlock()
	if persistenceStore == nil {
		return nil
	}
	if err := persistenceStore.flush(); err != nil {
		return err
	}
	err := persistenceStore.db.Close()
	persistenceStore = nil
	return err
}

func (store *sqlitePersistence) flush() error {
	snapshot, err := snapshotPersistence()
	if err != nil {
		return err
	}
	return store.writeSnapshot(snapshot)
}

// Compare with the last committed snapshot before starting a transaction. Only
// changed rows, new logs, and explicit removals produce SQL writes.
func (store *sqlitePersistence) writeSnapshot(snapshot persistenceSnapshot) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	next := make(map[monitorKey][]byte, len(snapshot.Rows))
	var changed []persistedMonitorRow
	var removed []monitorKey
	for _, row := range snapshot.Rows {
		key := monitorKey{row.Kind, row.Key}
		next[key] = row.Data
		if !bytes.Equal(store.rows[key], row.Data) {
			changed = append(changed, row)
		}
	}
	for key := range store.rows {
		if _, ok := next[key]; !ok {
			removed = append(removed, key)
		}
	}
	// Keep stable log IDs even when prepending and trimming the bounded history.
	available := make(map[TimedEntry][]persistedLog)
	for _, entry := range store.logs {
		available[entry.Entry] = append(available[entry.Entry], entry)
	}
	nextLogs := make([]persistedLog, len(snapshot.Logs))
	retained := make(map[int64]bool)
	sequence := store.sequence
	var added []persistedLog
	for i := len(snapshot.Logs) - 1; i >= 0; i-- {
		entry := snapshot.Logs[i]
		entry.Timestamp = entry.Timestamp.UTC()
		matches := available[entry]
		if len(matches) > 0 {
			nextLogs[i] = matches[len(matches)-1]
			available[entry] = matches[:len(matches)-1]
			retained[nextLogs[i].Sequence] = true
		} else {
			sequence++
			nextLogs[i] = persistedLog{sequence, entry}
			added = append(added, nextLogs[i])
		}
	}
	var removedLogs []int64
	for _, entry := range store.logs {
		if !retained[entry.Sequence] {
			removedLogs = append(removedLogs, entry.Sequence)
		}
	}
	if len(changed)+len(removed)+len(added)+len(removedLogs) == 0 {
		return nil
	}
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, key := range removed {
		if _, err := tx.Exec("DELETE FROM monitor_state WHERE kind=? AND key=?", key.Kind, key.Key); err != nil {
			return err
		}
	}
	for _, row := range changed {
		if _, err := tx.Exec(`INSERT INTO monitor_state(kind,key,data,updated_at) VALUES (?,?,?,?) ON CONFLICT(kind,key) DO UPDATE SET data=excluded.data, updated_at=excluded.updated_at`, row.Kind, row.Key, row.Data, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	for _, id := range removedLogs {
		if _, err := tx.Exec("DELETE FROM log_history WHERE sequence=?", id); err != nil {
			return err
		}
	}
	for _, entry := range added {
		if _, err := tx.Exec("INSERT INTO log_history VALUES (?,?,?)", entry.Sequence, entry.Entry.Timestamp.Format(time.RFC3339Nano), entry.Entry.Value); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	store.rows = next
	store.logs = nextLogs
	store.sequence = sequence
	return nil
}

func snapshotPersistence() (persistenceSnapshot, error) {
	monitorData.RLock()
	defer monitorData.RUnlock()
	logLock.RLock()
	defer logLock.RUnlock()
	return snapshotState(getConfig(), &monitorData, webEdits, logHistory)
}

func snapshotState(c *Config, state *MonitorData, edits map[monitorKey]webEdit, logs []TimedEntry) (persistenceSnapshot, error) {
	snapshot := persistenceSnapshot{Logs: append([]TimedEntry(nil), logs...)}
	appendRow := func(kind, key string, value interface{}) error {
		data, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("serialize %s/%s: %w", kind, key, err)
		}
		snapshot.Rows = append(snapshot.Rows, persistedMonitorRow{kind, key, data})
		return nil
	}
	for key, value := range state.MQTT {
		saved := *value
		if len(saved.History) > c.Monitor.MQTT.History {
			saved.History = saved.History[len(saved.History)-c.Monitor.MQTT.History:]
		}
		if err := appendRow("mqtt", key, &saved); err != nil {
			return snapshot, err
		}
	}
	for key, value := range state.Ping {
		if err := appendRow("ping", key, value); err != nil {
			return snapshot, err
		}
	}
	for key, value := range state.HTTP {
		if err := appendRow("http", key, value); err != nil {
			return snapshot, err
		}
	}
	for key, value := range state.Exec {
		if err := appendRow("exec", key, value); err != nil {
			return snapshot, err
		}
	}
	for key, value := range edits {
		if err := appendRow("edit/"+key.Kind, key.Key, value); err != nil {
			return snapshot, err
		}
	}
	if len(snapshot.Logs) > c.LogSize {
		snapshot.Logs = snapshot.Logs[:c.LogSize]
	}
	return snapshot, nil
}

func mqttAverageTransmit(history []TimedEntry) float64 {
	if len(history) < 2 {
		return 0
	}
	var total float64
	for i := 1; i < len(history); i++ {
		total += history[i].Timestamp.Sub(history[i-1].Timestamp).Seconds()
	}
	return total / float64(len(history)-1)
}

func validTimeout(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0
}

func configurePersistence(path string) error {
	c := *getConfig()
	c.Persistence.DB = path
	return configurePersistenceForConfig(&c, true)
}

// Stage and commit the replacement before publishing any new configuration or
// monitor state. On errors, the previous live configuration and store survive.
func configurePersistenceForConfig(c *Config, keepEdits bool) error {
	if err := validateConfig(c); err != nil {
		return err
	}
	resolved, err := resolvePersistencePath(c.Persistence.DB)
	if err != nil {
		return err
	}
	persistenceMu.Lock()
	defer persistenceMu.Unlock()
	old := persistenceStore
	// Aliases and symlinks to the same database are a reload, not a switch.
	if old != nil && resolved != "" && old.path != resolved {
		oldInfo, oldErr := os.Stat(old.path)
		newInfo, newErr := os.Stat(resolved)
		if oldErr == nil && newErr == nil && os.SameFile(oldInfo, newInfo) {
			resolved = old.path
		}
	}
	next := old
	switching := (old == nil && resolved != "") || (old != nil && old.path != resolved)
	var restored *MonitorData
	var restoredEdits map[monitorKey]webEdit
	var restoredLogs []TimedEntry
	if switching {
		next, err = openSQLitePersistence(resolved)
		if err != nil {
			return err
		}
		if next != nil {
			restored, restoredEdits, restoredLogs, err = decodePersistence(next)
			if err != nil {
				next.db.Close()
				return err
			}
		}
	}
	discard := func(err error) error {
		if switching && next != nil {
			next.db.Close()
		}
		return err
	}
	monitorData.Lock()
	defer monitorData.Unlock()
	logLock.Lock()
	defer logLock.Unlock()
	if switching && old != nil {
		snapshot, err := snapshotState(getConfig(), &monitorData, webEdits, logHistory)
		if err != nil {
			return discard(err)
		}
		if err := old.writeSnapshot(snapshot); err != nil {
			return discard(err)
		}
	}
	state := copyMonitorData(&monitorData)
	edits := copyWebEdits(webEdits)
	logs := append([]TimedEntry(nil), logHistory...)
	loadedFromDB := false
	if restored != nil && (len(next.rows) > 0 || len(next.logs) > 0) {
		state = restored
		edits = restoredEdits
		logs = restoredLogs
		loadedFromDB = true
		// An explicit database switch loads the selected database's overrides.
		keepEdits = true
	}
	reconcileState(c, state, edits, keepEdits)
	if len(logs) > c.LogSize {
		logs = logs[:c.LogSize]
	}
	if next != nil {
		snapshot, err := snapshotState(c, state, edits, logs)
		if err != nil {
			return discard(err)
		}
		if err := next.writeSnapshot(snapshot); err != nil {
			return discard(err)
		}
	}
	publishMonitorData(state, edits)
	monitorGeneration++
	// Reset the silence baseline when state is (re)loaded from a database so that
	// restored targets are not immediately reported as timed out.
	if !configurationLoaded || loadedFromDB {
		stateEpoch = time.Now()
	}
	logHistory = logs
	configLock.Lock()
	config = c
	configLock.Unlock()
	persistenceStore = next
	if switching && old != nil {
		if err := old.db.Close(); err != nil {
			fmt.Printf("[persistence] unable to close old database: %s\n", err)
		}
	}
	return nil
}
