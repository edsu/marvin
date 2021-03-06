package web

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"go/build"
	"html/template"
	"io"
	"log"
	"net/http"
	"os"
	"path"

	"code.google.com/p/go.net/websocket"
	"github.com/eikeon/marvin"
)

var pkg struct {
	Version string `json:"version"`
}
var Root string
var site *template.Template
var templates = make(map[string]*template.Template)

func init() {
	if p, err := build.Default.Import("github.com/eikeon/marvin/web", "", build.FindOnly); err == nil {
		Root = p.Dir
	} else {
		log.Println("WARNING: could not import package:", err)
	}

	if j, err := os.OpenFile(path.Join(Root, "package.json"), os.O_RDONLY, 0666); err == nil {
		dec := json.NewDecoder(j)
		if err = dec.Decode(&pkg); err != nil {
			log.Println("WARNING: could not decode package.json", err)
		}
		j.Close()
	} else {
		log.Println("WARNING: could not open package.json", err)
	}

}

type longExpireHandler struct {
	h http.Handler
}

func longExpire(h http.Handler) http.Handler {
	return &longExpireHandler{h}
}

func (le *longExpireHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ttl := int64(86400)
	w.Header().Set("Cache-Control", fmt.Sprintf("max-age=%d", ttl))
	le.h.ServeHTTP(w, r)
}

func getTemplate(name string) *template.Template {
	if t, ok := templates[name]; ok {
		return t
	} else {
		if site == nil {
			site = template.Must(template.ParseFiles(path.Join(Root, "templates/site.html")))
		}
		t, err := site.Clone()
		if err != nil {
			log.Fatal("cloning site: ", err)
		}
		t = template.Must(t.ParseFiles(path.Join(Root, name)))
		templates[name] = t
		return t
	}
}

type templateData map[string]interface{}

func writeTemplate(t *template.Template, d templateData, w http.ResponseWriter) {
	var bw bytes.Buffer
	h := md5.New()
	mw := io.MultiWriter(&bw, h)
	err := t.ExecuteTemplate(mw, "html", d)
	if err == nil {
		w.Header().Set("ETag", fmt.Sprintf(`"%x"`, h.Sum(nil)))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", bw.Len()))
		w.Write(bw.Bytes())
	} else {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func handleTemplate(prefix, name string, data templateData) {
	t := getTemplate("templates/" + name + ".html")
	http.HandleFunc(prefix, func(w http.ResponseWriter, req *http.Request) {
		d := templateData{}
		d["Title"] = name
		d["Version"] = pkg.Version
		if data != nil {
			for k, v := range data {
				d[k] = v
			}
		}
		if req.URL.Path == prefix {
			d["Found"] = true
		} else {
			w.Header().Set("Cache-Control", "max-age=10, must-revalidate")
			w.WriteHeader(http.StatusNotFound)
		}
		writeTemplate(t, d, w)
	})
}

type message map[string]interface{}

type stateServer struct {
	marvin *marvin.Marvin
}

func (s stateServer) wsHandler(ws *websocket.Conn) {
	stateChanges := make(chan marvin.State, 10)
	s.marvin.Register(&stateChanges)
	defer func() { s.marvin.Unregister(&stateChanges) }()
	go func() {
		for {
			var msg message
			if err := websocket.JSON.Receive(ws, &msg); err == nil {
				if msg["message"] != nil {
					req := ws.Request()
					who := req.RemoteAddr
					if req.TLS != nil {
						for _, c := range req.TLS.PeerCertificates {
							who = c.Subject.CommonName
						}
					}
					s.marvin.Do(who, msg["message"].(string), "web")
				} else {
					log.Printf("ignoring: %#v\n", msg)
				}
			} else {
				log.Println("State Websocket receive err:", err)
				return
			}
		}

	}()

	for state := range stateChanges {
		if err := websocket.JSON.Send(ws, state); err != nil {
			log.Println("State Websocket send err:", err)
			break
		}
	}
	ws.Close()
}

func AddHandlers(m *marvin.Marvin) {
	handleTemplate("/", "home", templateData{"Marvin": m})
	handleTemplate("/schedule/", "schedule", templateData{"Marvin": m})
	handleTemplate("/senses/", "senses", templateData{"Marvin": m})
	handleTemplate("/lightstates/", "lightstates", templateData{"Marvin": m})
	handleTemplate("/transitions/", "transitions", templateData{"Marvin": m})
	handleTemplate("/messages/", "messages", templateData{"Marvin": m})

	fs := longExpire(http.FileServer(http.Dir(path.Join(Root, "static/"))))
	http.Handle("/"+pkg.Version+"/", fs)

	http.HandleFunc("/post", func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "POST" {
			if err := req.ParseForm(); err == nil {
				name, ok := req.Form["do_transition"]
				if ok {
					who := req.RemoteAddr
					m.Do(who, name[0], "web")
				}
			}
			// TODO: write a response
		} else {
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	s := &stateServer{marvin: m}
	http.Handle("/state", websocket.Handler(s.wsHandler))
}
