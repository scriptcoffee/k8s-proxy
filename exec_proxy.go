package main

import (
	"io"
	"os"
	"log"
	"flag"
	"time"
	"strings"
	"net/http"
	"path/filepath"
	b64 "encoding/base64"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
)

var (
	config 		*rest.Config
	clientset 	*kubernetes.Clientset
	upgrader 	= websocket.Upgrader{}
	addr    	= flag.String("addr", "127.0.0.1:8888", "http service address")
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Maximum message size allowed from peer.
	maxMessageSize = 8192

	// Time to wait before closing connection due to inactivity
	//readTimeout = 5 * time.Minute
	readTimeout = 5 * time.Minute

	// Time to wait before force close on connection.
	closeGracePeriod = 10 * time.Second
)

func main() {
	//Load Kubernetes config
	var kubeconfig *string
	if home := homeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "execConfig"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()

	// use the current context in kubeconfig
	var err error
	config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	if err != nil {
		panic(err.Error())
	}

	// create the clientset
	clientset, err = kubernetes.NewForConfig(config)


	//Set up API
	router := mux.NewRouter()
	router.HandleFunc("/api/v1/namespaces/{namespace}/pods/{podName}/exec", serveWs).Methods("GET")
	router.HandleFunc("/api/v1/namespaces/{namespace}/pods/{podName}/exec", serveWs).Methods("POST")

	log.Fatal(http.ListenAndServe(*addr, router))
}

func serveWs(w http.ResponseWriter, r *http.Request) {
	//Upgrade incoming client connection to ws
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("upgrade:", err)
		return
	}
	defer ws.Close()

	//Get container details
	params := mux.Vars(r)
	vals := r.URL.Query()
	namespace 			:= params["namespace"]
	podName 			:= params["podName"]

	var containerName string
	containerNames, ok 	:= vals["container"]
	if ok && len(containerNames) >= 1 {
		containerName = containerNames[0]
	}

	//Open connection to k8s/OpenShift API
	restClient := clientset.CoreV1().RESTClient()

	var req *rest.Request
	commands := []string{"/bin/sh", "-i"}

	if len(containerName) != 0 {
		req = restClient.Post().
			Namespace(namespace).
			Resource("pods").
			Name(podName).
			SubResource("exec").
			Param("container", containerName).
			Param("stdin", "true").
			Param("stdout", "true").
			Param("stderr", "true").
			Param("tty", "true")
	} else {
		req = restClient.Post().
			Namespace(namespace).
			Resource("pods").
			Name(podName).
			SubResource("exec").
			Param("stdin", "true").
			Param("stdout", "true").
			Param("stderr", "true").
			Param("tty", "true")
	}

	for _, command := range commands {
		req.Param("command", command)
	}


	executor, err := remotecommand.NewSPDYExecutor(config, http.MethodPost, req.URL())
	if err != nil {
		errToWs(ws, err.Error())
		return
	}

	writer := newChanWriter()

	dp := newDataPipe()
	go handleWriter(writer, ws)
	go handleReader(ws, dp)


	err = executor.Stream(remotecommand.StreamOptions{
		Stdin:             dp,     //io.Reader
		Stdout:            writer, //io.Writer
		Stderr:            writer, //io.Writer
		TerminalSizeQueue: nil,
	})

	if err != nil {
		errToWs(ws, err.Error())
		return
	}
}

//Send error msg to ws client
func errToWs(ws *websocket.Conn, err string) {
	ws.SetWriteDeadline(time.Now().Add(writeWait))
	ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseUnsupportedData, err))
	time.Sleep(closeGracePeriod)
}

//handleReader reads, decodes and forwards messages from ws connection to container stdin
func handleReader(ws *websocket.Conn, dp *dataPipe) {
	defer ws.Close()
	ws.SetReadLimit(maxMessageSize)

	for {
		ws.SetReadDeadline(time.Now().Add(readTimeout))
		_, message, err := ws.ReadMessage()
		if err != nil {
			if strings.Contains(err.Error(), "timeout") {
				errToWs(ws, "Disconnected due to inactivity")
			} else {
				errToWs(ws, err.Error())
			}

			break
		}

		data := make([]byte, len(message))
		n, err := b64.StdEncoding.Decode(data, message[1:])
		if err != nil {
			errToWs(ws, err.Error())
			break
		}

		_, err = dp.receiveData(data[:n])
		if err != nil {
			errToWs(ws, err.Error())
			break
		}
	}
}

//handleWriter receives, encodes and forwards container output to ws connection
func handleWriter(w *chanWriter, ws *websocket.Conn) {
	for c := range w.Chan() {
		bRead := []byte{c}

		ws.SetWriteDeadline(time.Now().Add(writeWait))
		if err := ws.WriteMessage(websocket.TextMessage, []byte("1"+b64.StdEncoding.EncodeToString(bRead))); err != nil {
			errToWs(ws, err.Error())
			ws.Close()
			break
		}
	}

	ws.SetWriteDeadline(time.Now().Add(writeWait))
	ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	time.Sleep(closeGracePeriod)
	ws.Close()
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return os.Getenv("USERPROFILE") // windows
}

//Used to receive container output
type chanWriter struct {
	ch chan byte
}

func newChanWriter() *chanWriter {
	return &chanWriter{make(chan byte, 1024)}
}

func (w *chanWriter) Chan() <-chan byte {
	return w.ch
}

func (w *chanWriter) Write(p []byte) (int, error) {
	n := 0
	for _, b := range p {
		w.ch <- b
		n++
	}
	return n, nil
}

func (w *chanWriter) Close() error {
	close(w.ch)
	return nil
}

//Providing a pipe to relay messages between ws and container
type dataPipe struct {
	r io.Reader
	w io.WriteCloser
}

func newDataPipe() *dataPipe {
	r, w := io.Pipe()
	return &dataPipe{r, w}
}

func (p *dataPipe) receiveData(data []byte) (int, error) {
	return p.w.Write(data)
}

func (p *dataPipe) Read(data []byte) (int, error) {
	i, e := p.r.Read(data)
	return i, e
}

func (p *dataPipe) Close() error {
	return p.w.Close()
}
