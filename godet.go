package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"time"

	"github.com/gobs/args"
	"github.com/gobs/httpclient"
	"golang.org/x/net/websocket"
)

func decode(resp *httpclient.HttpResponse, v interface{}) error {
	err := json.NewDecoder(resp.Body).Decode(v)
	resp.Close()

	return err
}

//
// DevTools version info
//
type Version struct {
	Browser         string `json:"Browser"`
	ProtocolVersion string `json:"Protocol-Version"`
	UserAgent       string `json:"User-Agent"`
	V8Version       string `json:"V8-Version"`
	WebKitVersion   string `json:"WebKit-Version"`
}

func (v *Version) String() string {
	return fmt.Sprintf("Browser: %v\n"+
		"Protocol Version: %v\n"+
		"User Agent: %v\n"+
		"V8 Version: %v\n"+
		"WebKit Version: %v\n",
		v.Browser,
		v.ProtocolVersion,
		v.UserAgent,
		v.V8Version,
		v.WebKitVersion)
}

//
// Chrome open tab or page
//
type Tab struct {
	Id          string `json:"id"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Title       string `json:"title"`
	Url         string `json:"url"`
	WsUrl       string `json:"webSocketDebuggerUrl"`
	DevUrl      string `json:"devtoolsFrontendUrl"`
}

func (t Tab) String() string {
	return fmt.Sprintf("Id: %v\n"+
		"Type: %v\n"+
		"Description: %v\n"+
		"Title: %v\n"+
		"Url: %v\n"+
		"WebSocket Url: %v\n"+
		"Devtools Url: %v\n",
		t.Id,
		t.Type,
		t.Description,
		t.Title,
		t.Url,
		t.WsUrl,
		t.DevUrl)
}

//
// RemoteDebugger
//
type RemoteDebugger struct {
	http   *httpclient.HttpClient
	ws     *websocket.Conn
	reqid  int
	closed bool

	responses map[int]chan json.RawMessage
	r_lock    sync.Mutex
}

//
// Connect to the remote debugger and return `RemoteDebugger` object
//
func Connect(port string) (*RemoteDebugger, error) {
	remote := &RemoteDebugger{
		http:      httpclient.NewHttpClient("http://" + port),
		responses: map[int]chan json.RawMessage{},
	}

	// check http connection
	tabs, err := remote.TabList("")
	if err != nil {
		return nil, err
	}

	var wsUrl string

	for _, tab := range tabs {
		if tab.WsUrl != "" {
			wsUrl = tab.WsUrl
			break
		}
	}

	// check websocket connection
	if remote.ws, err = websocket.Dial(wsUrl, "", "http://localhost"); err != nil {
		return nil, err
	}

	go remote.readMessages()
	return remote, nil
}

func (remote *RemoteDebugger) Close() error {
	remote.closed = true
	return remote.ws.Close()
}

type wsParams map[string]interface{}

type wsMessage struct {
	Id     int             `json:"id"`
	Result json.RawMessage `json:"result"`

	Method string          `json:"Method"`
	Params json.RawMessage `json:"Params"`
}

func (remote *RemoteDebugger) sendRequest(method string, params wsParams) (json.RawMessage, error) {
	remote.r_lock.Lock()
	reqid := remote.reqid
	remote.responses[reqid] = make(chan json.RawMessage, 1)
	remote.reqid++
	remote.r_lock.Unlock()

	command := map[string]interface{}{
		"id":     reqid,
		"method": method,
		"params": params,
	}

	bytes, err := json.Marshal(command)
	if err != nil {
		return nil, err
	}

	log.Println("send", string(bytes))

	_, err = remote.ws.Write(bytes)
	if err != nil {
		return nil, err
	}

	res := <-remote.responses[reqid]
	remote.r_lock.Lock()
	remote.responses[reqid] = nil
	remote.r_lock.Unlock()

	return res, nil
}

func (remote *RemoteDebugger) readMessages() {
	buf := make([]byte, 4096)
	var bytes []byte

	for !remote.closed {
		if n, err := remote.ws.Read(buf); err != nil {
			log.Println("read error", err)
			if err == io.EOF {
				break
			}
		} else {
			if n > 0 {
				bytes = append(bytes, buf[:n]...)

				// hack to check end of message
				if bytes[0] == '{' && bytes[len(bytes)-1] != '}' {
					continue
				}
			}

			var message wsMessage

			//
			// unmarshall message
			//
			if err := json.Unmarshal(bytes, &message); err != nil {
				log.Println("error unmarshaling", string(bytes), len(bytes), err)
			} else if message.Method != "" {
				//
				// should be an event notification
				//
				log.Println("EVENT", message.Method, string(message.Params))
			} else {
				//
				// should be a method reply
				//
				remote.r_lock.Lock()
				ch := remote.responses[message.Id]
				remote.r_lock.Unlock()

				if ch != nil {
					ch <- message.Result
				}
			}

			bytes = nil
		}
	}
}

//
// Return various version info (protocol, browser, etc.)
//
func (remote *RemoteDebugger) Version() (*Version, error) {
	resp, err := remote.http.Get("/json/version", nil, nil)
	if err != nil {
		return nil, err
	}

	var version Version

	if err = decode(resp, &version); err != nil {
		return nil, err
	}

	return &version, nil
}

//
// Return the list of open tabs/page
//
// If filter is not empty only tabs of the specified type are returned (i.e. "page")
//
func (remote *RemoteDebugger) TabList(filter string) ([]*Tab, error) {
	resp, err := remote.http.Get("/json/list", nil, nil)
	if err != nil {
		return nil, err
	}

	var tabs []*Tab

	if err = decode(resp, &tabs); err != nil {
		return nil, err
	}

	if filter == "" {
		return tabs, nil
	}

	var filtered []*Tab

	for _, t := range tabs {
		if t.Type == filter {
			filtered = append(filtered, t)
		}
	}

	return filtered, nil
}

//
// Activate specified tab
//
func (remote *RemoteDebugger) ActivateTab(tab *Tab) error {
	resp, err := remote.http.Get("/json/activate/"+tab.Id, nil, nil)
	resp.Close()
	return err
}

//
// Close specified tab
//
func (remote *RemoteDebugger) CloseTab(tab *Tab) error {
	resp, err := remote.http.Get("/json/close/"+tab.Id, nil, nil)
	resp.Close()
	return err
}

//
// Create a new tab
//
func (remote *RemoteDebugger) NewTab(url string) (*Tab, error) {
	params := map[string]interface{}{}
	if url != "" {
		params["url"] = url
	}
	resp, err := remote.http.Get("/json/new", params, nil)
	if err != nil {
		return nil, err
	}

	var tab Tab
	if err = decode(resp, &tab); err != nil {
		return nil, err
	}

	return &tab, nil
}

func (remote *RemoteDebugger) getDomains() error {
	res, err := remote.sendRequest("Schema.getDomains", nil)
	if res != nil {
		log.Println(" ", string(res))
	}

	return err
}

func (remote *RemoteDebugger) Navigate(url string) error {
	res, err := remote.sendRequest("Page.navigate", wsParams{
		"url": url,
	})

	if res != nil {
		log.Println(" ", string(res))
	}

	return err
}

func (remote *RemoteDebugger) events(domain string, enable bool) error {
	method := domain

	if enable {
		method += ".enable"
	} else {
		method += ".disable"
	}

	res, err := remote.sendRequest(method, nil)
	if res != nil {
		log.Println(" ", string(res))
	}

	return err
}

func (remote *RemoteDebugger) DOMEvents(enable bool) error {
	return remote.events("DOM", enable)
}

func (remote *RemoteDebugger) PageEvents(enable bool) error {
	return remote.events("Page", enable)
}

func (remote *RemoteDebugger) NetworkEvents(enable bool) error {
	return remote.events("Network", enable)
}

func (remote *RemoteDebugger) RuntimeEvents(enable bool) error {
	return remote.events("Runtime", enable)
}

func runCommand(commandString string) error {
	parts := args.GetArgs(commandString)
	cmd := exec.Command(parts[0], parts[1:]...)
	err := cmd.Start()
	if err == nil {
		time.Sleep(time.Second) // give the app some time to start
	} else {
		log.Println("command start", err)
	}

	return err
}

func main() {
	cmd := flag.String("cmd", "open /Applications/Google\\ Chrome.app --args --remote-debugging-port=9222 --disable-extensions --headless about:blank", "command to execute to start the browser")
	port := flag.String("port", "localhost:9222", "Chrome remote debugger port")
	filter := flag.String("filter", "page", "filter tab list")
	page := flag.String("page", "http://httpbin.org", "page to load")
	flag.Parse()

	if *cmd != "" {
		runCommand(*cmd)
	}

	remote, err := Connect(*port)
	if err != nil {
		log.Fatal("connect", err)
	}

	defer remote.Close()

	fmt.Println()
	fmt.Println("Version:")
	fmt.Println(remote.Version())

	fmt.Println()
	tabs, err := remote.TabList(*filter)
	if err != nil {
		log.Fatal("cannot get list of tabs: ", err)
	}

	fmt.Println(tabs)

	fmt.Println()
	fmt.Println(remote.getDomains())

	remote.PageEvents(true)
	remote.DOMEvents(true)
	remote.RuntimeEvents(true)
	remote.NetworkEvents(true)

	l := len(tabs)
	if l > 0 {
		remote.ActivateTab(tabs[l-1])

		fmt.Println()
		fmt.Println(remote.Navigate(*page))
	}

	time.Sleep(60 * time.Second)
}
