package main

// platform wishes:
//   - attached dyno, close connection quickly, don't freak out
//   - attached dyno with no tty
//   - one-off dyno with a one-off slug
//   - faster pls
//     - api
//     - ps launch

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/kr/hpush/msg"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"text/template"
	"time"
)

const (
	MaxTarSize   = 2 * 1000 * 1000
	MatchTimeout = 15 * time.Second
)

var (
	Inbound = make(chan *iconn)
	Waiting = make(chan *wconn)
	Cancel  = make(chan string)
)

var (
	apiURL = "https://api.heroku.com"
)

var (
	builderPath = mustLookPath("builder")
	builderBin  = mustReadFile(builderPath)
	builderSha1 = sha1sum(builderBin)
)

var (
	tmpDir = os.TempDir()
)

func main() {
	log.SetFlags(log.Lshortfile)
	if s := os.Getenv("HEROKU_API_URL"); s != "" {
		apiURL = strings.TrimRight(s, "/")
	}
	log.Println("selfURL", selfURL())
	go match()
	handlePrefix("/push/", errHandler{handlePush})
	handlePrefix("/conn/", errHandler{handleConn})
	http.HandleFunc("/builder", handleBuilder)
	listen := ":" + os.Getenv("PORT")
	if listen == ":" {
		listen = ":8000"
	}
	err := http.ListenAndServe(listen, nil)
	if err != nil {
		panic(err)
	}
}

func match() {
	wait := make(map[string]*wconn)
	for {
		select {
		case wc := <-Waiting:
			wait[wc.ID] = wc
		case id := <-Cancel:
			delete(wait, id)
		case ic := <-Inbound:
			if wc := wait[ic.ID]; wc == nil {
				ic.c.Close()
			} else {
				delete(wait, ic.ID)
				wc.c <- ic.c
			}
		}
	}
}

func handleBuilder(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, builderPath)
}

func handleConn(w http.ResponseWriter, r *http.Request) error {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return errors.New("web server doesn't support hijacking")
	}
	c, _, err := hj.Hijack()
	if err != nil {
		return err
	}
	Inbound <- &iconn{ID: r.URL.Path, c: c}
	return nil
}

func handlePush(w http.ResponseWriter, r *http.Request) error {
	app := r.URL.Path
	_, key := getBasicAuth(r.Header.Get("Authorization"))
	if key == "" {
		http.Error(w, "unauthorized", 401)
		return nil
	}
	if r.ContentLength > MaxTarSize {
		http.Error(w, "too big", 413)
		return nil
	}

	wc, err := startBuilder(key, app)
	if err != nil {
		return err
	}

	// while the dyno is spinning up, read the body
	f, err := spool(http.MaxBytesReader(w, r.Body, MaxTarSize))
	if err != nil {
		return fmt.Errorf("spool: %v", err)
	}
	fi, _ := f.Stat()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	fprintf(w, "started build dyno %s\n", wc.psname)

	slugURL := ""

	slug, procfile := waitBuild(w, wc, slugURL, f, fi.Size())
	if slug == nil || procfile == nil {
		fprintf(w, "error\n")
		return nil
	}
	fi, _ = slug.Stat()
	fprintf(w, "got slug %d bytes\n", fi.Size())
	fprintf(w, "releasing\n")
	name, err := release(key, app, slug, fi.Size(), procfile)
	if err != nil {
		fprintf(w, "release err %v\n", err)
		return nil
	}
	fprintf(w, "done, release %s\n", name)
	return nil
}

func waitBuild(w io.Writer, wc *wconn, slugURL string, bun *os.File, size int64) (slug *os.File, procfile []byte) {
	defer func() { Cancel <- wc.ID }()
	//go io.Copy(ioutil.Discard, wc.runConn)
	go io.Copy(os.Stdout, wc.runConn)
	fprintf(w, "waiting for dyno\n")
	select {
	case bConn := <-wc.c:
		fprintf(w, "connected\n")
		//wc.runConn.Close()
		defer bConn.Close()
		slug, procfile = doBuild(w, bConn, slugURL, bun, size)
	case <-time.After(MatchTimeout):
		fprintf(w, "timeout\n")
		//wc.runConn.Close()
		log.Println("timeout:", wc.ID)
	}
	return
}

// read all of r into an unlinked temporary file
func spool(r io.Reader) (f *os.File, err error) {
	f, err = ioutil.TempFile(tmpDir, "spool")
	if err != nil {
		return
	}
	err = os.Remove(f.Name())
	if err != nil {
		f.Close()
		return nil, err
	}
	_, err = io.Copy(f, r)
	if err != nil {
		f.Close()
		return nil, err
	}
	f.Seek(0, 0)
	return
}

// Communication with builder proceeds as follows:
//   1. write slug url
//   2. write tarball
//   3. read user messages
//   4. read status
//   5. if success:
//      a. read slug
//      b. read procfile
func doBuild(w io.Writer, c net.Conn, slugURL string, bun io.Reader, size int64) (slug *os.File, procfile []byte) {
	err := msg.Write(c, msg.File, []byte(slugURL))
	if err != nil {
		log.Println("msg.Write:", err)
		fmt.Fprintln(w, "could not write slug url", err)
		fmt.Fprintln(w, "internal error")
		return nil, nil
	}
	err = msg.CopyN(c, msg.File, bun, size)
	if err != nil {
		log.Println("msg.CopyN:", err)
		fmt.Fprintln(w, "internal error")
		return nil, nil
	}
	t, m, err := msg.ReadFull(c)
	if err != nil {
		log.Println("msg.ReadFull:", err)
		fmt.Fprintln(w, "internal error")
		return nil, nil
	}
	fprintf(w, "starting build\n")
	for t == msg.User {
		w.Write(m)
		flush(w)
		t, m, err = msg.ReadFull(c)
		if err != nil {
			log.Println("msg.ReadFull:", err)
			fprintf(w, "\ninternal error\n")
			return nil, nil
		}
	}
	if t != msg.Status {
		log.Println("unexpected msg type", t)
		fprintf(w, "\ninternal error\n")
		return nil, nil
	}
	if m[0] == msg.Success {
		fprintf(w, "build ok\n")
		r, err1 := msg.ReadFile(c)
		if err1 != nil {
			log.Println("msg.ReadFile", err1)
			fprintf(w, "internal error\n")
			return nil, nil
		}
		slug, err = spool(r)
		if err != nil {
			log.Println("spool", err)
			fprintf(w, "internal error\n")
			return nil, nil
		}
		t, procfile, err = msg.ReadFull(c)
		if t != msg.File {
			log.Printf("expected file, got %d", t)
			fprintf(w, "internal error\n")
			return nil, nil
		}
	} else {
		fprintf(w, "\nbuild failed\n")
	}
	return slug, procfile
}

func fprintf(w io.Writer, format string, v ...interface{}) {
	fmt.Fprintf(w, format, v...)
	flush(w)
}

func flush(w io.Writer) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

var trampoline = template.Must(template.New("top").Parse(`
set -e
curl -s -o/tmp/builder ` + selfURL() + `/builder
printf "%s  %s" ` + builderSha1 + ` /tmp/builder >/tmp/sha1
sha1sum --status -c /tmp/sha1
chmod +x /tmp/builder
exec /tmp/builder ` + selfURL() + `/conn/{{.ID}}
`))

func startBuilder(key, app string) (wc *wconn, err error) {
	fmt.Println("starting builder for", app)
	name, runConn, err := psrun(key, app, "/bin/bash # app build")
	if err != nil {
		return nil, fmt.Errorf("psrun: %v", err)
	}
	fmt.Println("started", name)
	wc = &wconn{
		ID:      randhex(20),
		c:       make(chan net.Conn, 1),
		psname:  name,
		runConn: runConn,
	}
	fmt.Println("sending wc")
	Waiting <- wc
	fmt.Println("writing trampoline")
	if err = trampoline.Execute(runConn, wc); err != nil {
		return nil, err
	}
	return wc, nil
}

func psrun(key, app, cmd string) (name string, c net.Conn, err error) {
	var x struct {
		Name string
		URL  string `json:"attach_url"`
	}
	fmt.Println("psrun: post", app)
	err = apiPost(&x, key, "/apps/"+app+"/dynos", "", map[string]interface{}{
		"command": cmd,
		"attach":  true,
	})
	if err != nil {
		return "", nil, fmt.Errorf("api: %v", err)
	}
	fmt.Println("psrun: got resp")
	c, err = rendez(x.URL)
	return x.Name, c, err
}

func release(key, app string, slug *os.File, size int64, procfile []byte) (name string, err error) {
	var x struct{ Slug_put_url, Slug_put_key string }
	err = apiGet(&x, key, "/apps/"+app+"/releases/new", "application/json")
	if err != nil {
		return "", fmt.Errorf("api: %v: %s", err, "/apps/"+app+"/releases/new")
	}

	resp, err := put(x.Slug_put_url, slug, size)
	if err != nil {
		return "", fmt.Errorf("put: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 { // 200, 201, 202, etc
		return "", fmt.Errorf("bad s3 slug put status: %s", resp.Status)
	}

	var rel = map[string]interface{}{
		"slug_put_key":  x.Slug_put_key,
		"process_types": parseProcfile(procfile),
		"release_descr": "the desc",
		"head":          "foo", // what is this?
		//"config_vars":   map[string]string{},
		"addons":        []string{},
		"language_pack": "unknown",
		"run_deploy_hooks": true,
		"slug_version":  2,
		"stack":         "cedar",
	}
	var rresp struct {
		Release string
	}
	const jtype = "application/json"
	err = apiPost(&rresp, key, "/apps/"+app+"/releases", jtype, rel)
	if err != nil {
		err = fmt.Errorf("api: %v: %s", err, "/apps/"+app+"/releases")
	}
	name = rresp.Release
	return
}

func parseProcfile(b []byte) map[string]string {
	m := make(map[string]string)
	lines := bytes.Split(b, []byte{'\n'})
	for _, line := range lines {
		parts := bytes.SplitN(line, []byte{':'}, 2)
		if len(parts) == 2 {
			m[string(parts[0])] = strings.TrimSpace(string(parts[1]))
		}
	}
	return m
}

func put(url string, body io.Reader, size int64) (resp *http.Response, err error) {
	req, err := http.NewRequest("PUT", url, body)
	req.ContentLength = size
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func apiGet(v interface{}, key, path, acc string) error {
	req, err := http.NewRequest("GET", apiURL+path, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth("", key)
	req.Header.Set("User-Agent", "hpush")
	req.Header.Set("Content-Type", "application/json")
	if acc == "" {
		acc = "application/vnd.heroku+json; version=3"
	}
	req.Header.Set("Accept", acc)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 { // 200, 201, 202, etc
		return fmt.Errorf("bad status: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func apiPost(v interface{}, key, path, acc string, x interface{}) error {
	b, err := json.Marshal(x)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", apiURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.SetBasicAuth("", key)
	req.Header.Set("User-Agent", "hpush")
	req.Header.Set("Content-Type", "application/json")
	if acc == "" {
		acc = "application/vnd.heroku+json; version=3"
	}
	req.Header.Set("Accept", acc)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 { // 200, 201, 202, etc
		return fmt.Errorf("bad status: %s", resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func rendez(u string) (c net.Conn, err error) {
	up, err := url.Parse(u)
	if err != nil {
		return nil, fmt.Errorf("url: %v: %s", err, u)
	}
	fmt.Println("rendez dial")
	c, err = tls.Dial("tcp", up.Host, nil)
	if err != nil {
		return nil, err
	}
	fmt.Println("rendez send conn str")
	_, err = io.WriteString(c, up.Path[1:]+"\r\n")
	if err != nil {
		c.Close()
		return nil, err
	}
	fmt.Println("rendez read line")
	err = readline(c)
	if err != nil {
		c.Close()
		return nil, err
	}
	fmt.Println("rendez ok")
	return c, nil
}

func getBasicAuth(h string) (u, p string) {
	const prefix = "Basic "
	if !strings.HasPrefix(h, prefix) {
		return "", ""
	}
	b, err := base64.StdEncoding.DecodeString(h[len(prefix):])
	if err != nil {
		return "", ""
	}
	a := strings.SplitN(string(b), ":", 2)
	if len(a) != 2 {
		return a[0], ""
	}
	return a[0], a[1]
}

func selfURL() string {
	name, err := os.Hostname()
	if err != nil {
		panic(err)
	}
	addrs, err := net.LookupIP(name + ".int.dyno.rt.heroku.com")
	if err != nil {
		panic(err)
	}
	return "http://" + addrs[0].String() + ":" + os.Getenv("PORT")
}

func readline(r io.Reader) error {
	b := make([]byte, 1)
	for b[0] != '\n' {
		_, err := r.Read(b)
		if err != nil {
			return err
		}
	}
	return nil
}

func randhex(c int) string {
	b := make([]byte, c/2)
	n, err := io.ReadFull(rand.Reader, b)
	if n != len(b) || err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

type wconn struct {
	ID      string
	c       chan net.Conn
	psname  string
	runConn net.Conn
}

type iconn struct {
	ID string
	c  net.Conn
}

type errHandler struct {
	f func(w http.ResponseWriter, r *http.Request) error
}

func (h errHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	err := h.f(w, r)
	if err != nil {
		log.Println(err)
		http.Error(w, "internal error", 500)
		io.WriteString(w, err.Error())
	}
}

func handlePrefix(s string, h http.Handler) {
	http.Handle(s, http.StripPrefix(s, h))
}

func mustLookPath(name string) string {
	path, err := exec.LookPath(name)
	if err != nil {
		panic(err)
	}
	return path
}

func mustReadFile(filename string) []byte {
	b, err := ioutil.ReadFile(filename)
	if err != nil {
		panic(err)
	}
	return b
}

func sha1sum(p []byte) string {
	h := sha1.New()
	h.Write(p)
	return hex.EncodeToString(h.Sum(nil))
}
