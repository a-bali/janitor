package main

import (
	"fmt"
	"hash/fnv"
	"html/template"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"github.com/gobuffalo/packr"
	"gopkg.in/yaml.v2"
)

type Config struct {
	Debug bool
	Web   struct {
		Port int
	}
	Telegram struct {
		Token string
		Chat  int64
	}
	Gotify struct {
		Token  string
		Server string
	}
	MQTT struct {
		Server   string
		Port     int
		User     string
		Password string
		Topic    []string
	}
	MQTTHistory  int
	PingInterval int
	Ping         []struct {
		Name    string
		Address string
	}
	HTTP []struct {
		Name    string
		Address string
		Value   string
	}
	HTTPInterval int
	HTTPTimeout  int
}

type MQTTEntry struct {
	Payload   string
	Timestamp time.Time
}

type MQTTTopic struct {
	Id          uint64
	FirstSeen   time.Time
	LastSeen    time.Time
	LastError   time.Time
	LastPayload string
	History     []MQTTEntry
	AvgTransmit float64
	Status      int32
	Samples     int64
	Alerts      int64
}

type PingHost struct {
	Address    string
	LastOK     time.Time
	LastError  time.Time
	Status     int32
	TotalOK    int64
	TotalError int64
}

type HTTPHost struct {
	Id             uint64
	Address        string
	LastOK         time.Time
	LastError      time.Time
	LastValue      string
	LastErrorValue string
	Status         int32
	TotalOK        int64
	TotalError     int64
}

type PageData struct {
	MQTT       map[string]MQTTTopic
	Ping       map[string]PingHost
	HTTP       map[string]HTTPHost
	Timestamp  time.Time
	Uptime     time.Time
	Config     string
	LogHistory []LogEntry
}

type LogEntry struct {
	Timestamp time.Time
	Entry     string
}

var mqttTopics = make(map[string]MQTTTopic)
var pingHosts = make(map[string]PingHost)
var httpHosts = make(map[string]HTTPHost)
var mqttTopicsLock = &sync.RWMutex{}
var pingHostsLock = &sync.RWMutex{}
var httpHostsLock = &sync.RWMutex{}

var config = Config{}
var logHistory []LogEntry
var tgbot *tgbotapi.BotAPI
var uptime = time.Now()

func main() {
	if len(os.Args) != 2 {
		fmt.Println("Usage: " + os.Args[0] + " <configfile>")
		os.Exit(1)
	}

	loadConfig()

	if config.MQTT.Server != "" {
		connectMQTT()
		go checkMQTTStatus()
	}

	if len(config.Ping) > 0 {
		go startPing()
	}

	if len(config.HTTP) > 0 {
		go startHTTP()
	}

	http.HandleFunc("/", serveIndex)
	http.HandleFunc("/reload_config", reloadConfig)
	http.HandleFunc("/delete", deleteWebItem)

	log(fmt.Sprintf("Launching web server at :%d", config.Web.Port))
	panic(http.ListenAndServe(fmt.Sprintf(":%d", config.Web.Port), nil))
}

func loadConfig() {
	filename, _ := filepath.Abs(os.Args[1])
	yamlFile, err := ioutil.ReadFile(filename)
	if err != nil {
		panic(err)
	}

	err = yaml.Unmarshal(yamlFile, &config)
	if err != nil {
		panic(err)
	}

	if config.MQTTHistory == 0 {
		config.MQTTHistory = 10
	}
	if config.PingInterval == 0 {
		config.PingInterval = 60
	}
	if config.HTTPInterval == 0 {
		config.HTTPInterval = 60
	}
	if config.HTTPTimeout == 0 {
		config.HTTPTimeout = 5000
	}
	if config.Web.Port == 0 {
		config.Web.Port = 8080
	}

	debug("Loaded config: " + fmt.Sprintf("%+v", config))

	if config.Telegram.Token != "" {
		tgbot, err = tgbotapi.NewBotAPI(config.Telegram.Token)
		if err != nil {
			panic(err)
		}
		log("Connected to telegram bot")
	}
}

func startPing() {
	for {
		debug("Starting ping loop")
		for _, host := range config.Ping {
			pingHostsLock.RLock()
			e := pingHosts[host.Name]
			pingHostsLock.RUnlock()
			e.Address = host.Address
			if ping(host.Address) {
				debug("Ping OK for " + host.Address)
				if e.Status == 2 {
					notify("✓ Ping OK for " + host.Name + ", in error since " + relaTime(e.LastError) + " ago")
				}
				e.TotalOK++
				e.LastOK = time.Now()
				e.Status = 0
			} else {
				debug("Ping error for " + host.Address)
				if e.Status == 0 {
					e.Status++
				} else if e.Status == 1 {
					e.Status++
					e.LastError = time.Now()
					notify("⚠ Ping ERROR for " + host.Name + ", last seen " + relaTime(e.LastOK) + " ago")
				}
				e.TotalError++
			}
			pingHostsLock.Lock()
			pingHosts[host.Name] = e
			pingHostsLock.Unlock()
		}
		time.Sleep(time.Second * time.Duration(config.PingInterval))
	}
}

func startHTTP() {
	for {
		debug("Starting http loop")
		for _, host := range config.HTTP {
			httpHostsLock.RLock()
			e := httpHosts[host.Name]
			httpHostsLock.RUnlock()

			e.Id = hash(host.Address)
			e.Address = host.Address

			errValue := ""

			client := http.Client{
				Timeout: time.Millisecond * time.Duration(config.HTTPTimeout),
			}
			resp, err := client.Get(host.Address)
			if err != nil {
				errValue = err.Error()
			} else if resp.StatusCode != http.StatusOK {
				errValue = fmt.Sprintf("status code %d", resp.StatusCode)
			} else {
				defer resp.Body.Close()
				bodyBytes, bodyErr := ioutil.ReadAll(resp.Body)
				if bodyErr != nil {
					errValue = bodyErr.Error()
				} else {
					e.LastValue = string(bodyBytes)
					if host.Value != "" && !strings.Contains(e.LastValue, host.Value) {
						errValue = "Response does not match pattern"
					}
				}
			}

			if errValue == "" {
				debug("HTTP OK for " + host.Address)
				if e.Status == 2 {
					notify("✓ HTTP OK for " + host.Name + ", in error since " + relaTime(e.LastError) + " ago")
				}
				e.TotalOK++
				e.LastOK = time.Now()
				e.Status = 0
			} else {
				debug("HTTP error for " + host.Address)
				if e.Status == 0 {
					e.Status++
				} else if e.Status == 1 {
					e.Status++
					e.LastError = time.Now()
					notify("⚠ HTTP ERROR for " + host.Name + ", last ok " + relaTime(e.LastOK) + " ago")
				}
				e.TotalError++
				e.LastErrorValue = errValue
			}
			httpHostsLock.Lock()
			httpHosts[host.Name] = e
			httpHostsLock.Unlock()
		}
		time.Sleep(time.Second * time.Duration(config.HTTPInterval))
	}
}

func ping(host string) bool {
	cmd := exec.Command("x")
	if runtime.GOOS == "windows" {
		cmd = exec.Command("ping", "-n", "1", host)
	} else {
		cmd = exec.Command("ping", "-c", "1", host)
	}
	out, err := cmd.Output()
	if err != nil || strings.Contains(string(out), "Destination host unreachable") {
		return false
	}
	return true
}

func connectMQTT() {
	opts := mqtt.NewClientOptions()
	s := fmt.Sprintf("%s://%s:%d", "tcp", config.MQTT.Server, config.MQTT.Port)
	opts.AddBroker(s)
	if config.MQTT.User != "" {
		opts.SetUsername(config.MQTT.User)
		opts.SetPassword(config.MQTT.Password)
	}
	opts.OnConnect = func(c mqtt.Client) {
		topics := make(map[string]byte)
		for _, t := range config.MQTT.Topic {
			topics[t] = byte(0)
		}
		if token := c.SubscribeMultiple(topics, onMessageReceived); token.Wait() && token.Error() != nil {
			log(token.Error().Error())
		} else {
			log("Subscribed to MQTT topics: " + strings.Join(config.MQTT.Topic, ", "))
		}
	}

	client := mqtt.NewClient(opts)
	if token := client.Connect(); token.Wait() && token.Error() != nil {
		panic(token.Error().Error())
	} else {
		log("Connected to MQTT server at " + s)
	}
}

func onMessageReceived(client mqtt.Client, message mqtt.Message) {
	debug(message.Topic() + " " + string(message.Payload()))

	mqttTopicsLock.RLock()
	var e = mqttTopics[message.Topic()]
	mqttTopicsLock.RUnlock()

	e.Id = hash(message.Topic())
	e.History = append(e.History, MQTTEntry{string(message.Payload()), time.Now()})
	if len(e.History) > config.MQTTHistory {
		e.History = e.History[1:]
	}

	var total float64 = 0
	for i, v := range e.History {
		if i > 0 {
			total += v.Timestamp.Sub(e.History[i-1].Timestamp).Seconds()
		}
	}
	e.AvgTransmit = total / float64(len(e.History)-1)
	if e.FirstSeen.IsZero() {
		e.FirstSeen = time.Now()
	}
	e.LastSeen = time.Now()
	e.LastPayload = string(message.Payload())
	e.Samples++

	mqttTopicsLock.Lock()
	mqttTopics[message.Topic()] = e
	mqttTopicsLock.Unlock()
}

func checkMQTTStatus() {
	for {
		mqttTopicsLock.Lock()
		for topic, v := range mqttTopics {
			if v.AvgTransmit > 0 {
				switch elapsed := time.Now().Sub(v.LastSeen).Seconds(); {
				case elapsed > (v.AvgTransmit * 2):
					if v.Status != 2 {
						notify(fmt.Sprintf("⚠ MQTT ERROR: %s last seen %s ago (average interval %.2fs)", topic, relaTime(v.LastSeen), v.AvgTransmit))
						v.LastError = time.Now()
						v.Alerts++
					}
					v.Status = 2

				case elapsed > (v.AvgTransmit * 1.2):
					v.Status = 1

				default:
					if v.Status == 2 {
						notify(fmt.Sprintf("✓ MQTT OK for %s, in error since  %s ago", topic, relaTime(v.LastError)))
					}
					v.Status = 0
				}
				mqttTopics[topic] = v
			}
		}
		mqttTopicsLock.Unlock()
		time.Sleep(time.Second)
	}
}

func serveIndex(w http.ResponseWriter, r *http.Request) {
	debug("Web request " + r.RequestURI + " from " + r.RemoteAddr)

	box := packr.NewBox("./templates")
	s, err := box.FindString("index.html")
	if err != nil {
		panic(err)
	}

	tmpl, err := template.New("w").Funcs(template.FuncMap{
		"relaTime": relaTime,
	}).Parse(s)
	if err != nil {
		panic(err)
	}

	mqttTopicsLock.RLock()
	pingHostsLock.RLock()
	httpHostsLock.RLock()
	tmpl.Execute(w, PageData{mqttTopics, pingHosts, httpHosts,
		time.Now(), uptime, fmt.Sprintf("%+v", config), logHistory})
	mqttTopicsLock.RUnlock()
	pingHostsLock.RUnlock()
	httpHostsLock.RUnlock()
}

func reloadConfig(w http.ResponseWriter, r *http.Request) {
	log("Reloading config based on web request from " + r.RemoteAddr)
	loadConfig()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func deleteWebItem(w http.ResponseWriter, r *http.Request) {
	debug("Web request " + r.RequestURI + " from " + r.RemoteAddr)
	r.ParseForm()
	if len(r.Form["type"]) > 0 {
		switch r.Form["type"][0] {
		case "mqtt":
			if len(r.Form["name"]) > 0 {
				mqttTopicsLock.Lock()
				delete(mqttTopics, r.Form["name"][0])
				mqttTopicsLock.Unlock()
			}
		case "ping":
			if len(r.Form["name"]) > 0 {
				pingHostsLock.Lock()
				delete(pingHosts, r.Form["name"][0])
				pingHostsLock.Unlock()
			}
		case "http":
			if len(r.Form["name"]) > 0 {
				httpHostsLock.Lock()
				delete(httpHosts, r.Form["name"][0])
				httpHostsLock.Unlock()
			}
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func relaTime(t time.Time) string {
	if t.IsZero() {
		return "inf"
	}
	d := time.Since(t)

	day := time.Minute * 60 * 24
	s := ""
	if d > day {
		days := d / day
		d = d - days*day
		s = fmt.Sprintf("%dd", days)
	}

	if d < time.Second {
		return s + d.String()
	} else if m := d % time.Second; m+m < time.Second {
		return s + (d - m).String()
	} else {
		return s + (d + time.Second - m).String()
	}
}

func hash(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func log(s string) {
	fmt.Printf("[%s] %s\n", time.Now().Format("2006-01-02 15:04:05"), s)
	logHistory = append([]LogEntry{LogEntry{time.Now(), s}}, logHistory[:min(len(logHistory), 999)]...)
}

func debug(s string) {
	if config.Debug {
		log("(" + s + ")")
	}
}

func telegram(s string) {
	tgbot.Send(tgbotapi.NewMessage(config.Telegram.Chat, s))
}

func gotify(s string) {
	http.PostForm(config.Gotify.Server+"/message?token="+config.Gotify.Token,
		url.Values{"message": {s}, "title": {"Janitor alert"}})
}

func notify(s string) {
	log(s)
	if config.Telegram.Token != "" {
		telegram(s)
	}
	if config.Gotify.Token != "" {
		gotify(s)
	}
}
