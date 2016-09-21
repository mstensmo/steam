package h2ocluster

import (
	"github.com/cskr/pubsub"
	"github.com/h2oai/steamY/master/data"
	"os/exec"
	"syscall"
	"context"
	"github.com/pkg/errors"
	"time"
	"math/rand"
	"net/http"
	"github.com/gorilla/rpc"
	"github.com/gorilla/rpc/json"
	ejson "encoding/json"
	"net"
	"bytes"
)

// =================== API ===================
// Request sent to the driver to fetch properties
type Request struct {
	Names []string `msg:"names"`
}

// Reply from the driver with a map prop_name -> prop_value and the queue topic which requested it
type TopicArgs struct {
	Value map[string]interface{} `msg:"value"`
	Topic string `msg:"topic"`
}

// Like TopicArgs but is requested synchronously -> no need for topic
type Args struct {
	Value map[string]interface{} `msg:"value"`
}

// Reply to the driver (some status code)
type Reply struct {
	Code int `msg:"code"`
}

// Server handling driver request
type Handler struct {
	queue  *pubsub.PubSub
}

// Handles the initial response from the driver
func (t *Handler) Receive(r *http.Request, args *TopicArgs, reply *Reply) error {
	t.queue.Pub(args.Value, args.Topic)
	return nil
}

// =================== Client ===================

/**
	Gets value for all the properties found in `name` from a remote server.
	This call is blocking.
 */
func Get(addrs string, names []string) (map[string]interface{}, error) {
	url := addrs + "/h2ocluster"

	message, err := EncodeClientRequest("Driver.Get", names)
	if err != nil {
		return nil, errors.Wrapf(err, "failed encoding client request")
	}
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(message))
	if err != nil {
		return nil, errors.Wrapf(err, "failed building request")
	}
	req.Header.Set("Content-Type", "application/json-rpc")
	client := new(http.Client)
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.Wrapf(err, "failed sending request")
	}
	defer resp.Body.Close()

	var result Args
	err = json.DecodeClientResponse(resp.Body, &result)
	if err != nil {
		return nil, errors.Wrapf(err, "failed decoding response")
	}

	return result.Value, nil
}

// Stops a cluster
func Stop(addrs string) error {
	url := "http://" + addrs + "/h2ocluster"

	message, err := EncodeClientRequest("Driver.Stop", nil)
	if err != nil {
		return errors.Wrapf(err, "failed encoding client request")
	}
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(message))
	if err != nil {
		return errors.Wrapf(err, "failed building request")
	}
	req.Header.Set("Content-Type", "application/json-rpc")
	client := new(http.Client)

	resp, err := client.Do(req)
	if err != nil {
		return errors.Wrapf(err, "failed sending request")
	}
	defer resp.Body.Close()

	return nil
}

// Like in the json package but with jsonrpc version -> required by the Java client
type clientRequest struct {
	Method string `json:"method"`
	Params interface{} `json:"params"`
	Id uint64 `json:"id"`
	Version string `json:"jsonrpc"`
}

func EncodeClientRequest(method string, args interface{}) ([]byte, error) {
	c := &clientRequest{
		Method:  method,
		Params:  args,
		Id:      uint64(rand.Int63()),
		Version: "2.0",
	}

	return ejson.Marshal(c)
}

// =================== Cluster Manager/Server ===================

/**
	Object which communicates with all external clusters via RPC.
 */
type ClusterManager struct {
	// Used for Steam <-> Driver communication
	server *rpc.Server
	// PubSub which distributes cluster replies to appropriate threads
	Queue  *pubsub.PubSub
}

func NewClusterManager() *ClusterManager {
	// TODO come up with a better capacity
	queue := pubsub.New(42)
	return &ClusterManager{
		server: newServer(queue),
		Queue:  queue,
	}
}

// This server's address
var servAddrs string

func newServer(pubsub *pubsub.PubSub) *rpc.Server {
	s := rpc.NewServer()
	s.RegisterCodec(json.NewCodec(), "application/json-rpc")
	s.RegisterCodec(json.NewCodec(), "application/json-rpc;charset=UTF-8")
	handler := &Handler{queue:  pubsub}
	s.RegisterService(handler, "Handler")
	r := http.NewServeMux()
	r.Handle("/h2ocluster", s)

	ip, _ := externalIP()
	ln, _ := net.Listen("tcp", ip + ":0")

	go http.Serve(ln, r)

	servAddrs = ln.Addr().String()

	return s
}

func externalIP() (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue // interface down
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue // loopback interface
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return "", err
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip = ip.To4()
			if ip == nil {
				continue // not an ipv4 address
			}
			return ip.String(), nil
		}
	}
	return "", errors.New("are you connected to the network?")
}

/*
	Launches a new cluster of type engine with a given name. Returns pubsub topic for
	communications with it, the application name and its IP as a map.
 */
func (cm *ClusterManager) Launch(
	name string,
	engine data.Engine,
	uid, gid uint32,
	params ...string) (map[string]interface{}, error) {

	topic := "cluster_" + RandStr(5)

	ch := cm.Queue.Sub(topic)

	cmdArgs := append([]string{
		"-jar", engine.Location,
		"-Dmessaging.topic=" + topic,
		"-Dmessaging.addrs=" + servAddrs,
	}, params...)

	// Create context for killing process if exception encountered
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up hadoop job with user impersonation
	cmd := exec.CommandContext(ctx, "java", cmdArgs...)
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uid, Gid: gid}

	if err := cmd.Run(); err != nil {
		return nil, errors.Wrapf(err, "failed running command %s", cmd.Args)
	}

	select {
	case reply := <-ch:
		return reply.(map[string]interface{}), nil
	case <-time.After(time.Minute * 15):
		return nil, errors.New("timed out while waiting for cluster launch")
	}
}

// =================== UTILS ===================

func RandStr(strlen int) string {
	rand.Seed(time.Now().UTC().UnixNano())
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	r := make([]byte, strlen)
	for i := 0; i < strlen; i++ {
		r[i] = chars[rand.Intn(len(chars))]
	}
	return string(r)
}