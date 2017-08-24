package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mperham/faktory"
	"github.com/mperham/faktory/storage"
	"github.com/mperham/faktory/util"
)

var (
	EventHandlers = make([]func(*Server), 0)
)

type ServerOptions struct {
	Binding     string
	StoragePath string
	Password    string
}

type Server struct {
	Options    *ServerOptions
	Processed  int64
	Failures   int64
	pwd        string
	listener   net.Listener
	store      storage.Store
	scheduler  *SchedulerSubsystem
	pending    *sync.WaitGroup
	mu         sync.Mutex
	heartbeats map[string]*ClientWorker
}

// register a global handler to be called when the Server instance
// has finished booting but before it starts listening.
func OnStart(x func(*Server)) {
	EventHandlers = append(EventHandlers, x)
}

func NewServer(opts *ServerOptions) *Server {
	if opts.Binding == "" {
		opts.Binding = "localhost:7419"
	}
	if opts.StoragePath == "" {
		opts.StoragePath = fmt.Sprintf("%s.db", strings.Replace(opts.Binding, ":", "_", -1))
	}
	return &Server{
		Options:    opts,
		pwd:        "123456",
		pending:    &sync.WaitGroup{},
		mu:         sync.Mutex{},
		heartbeats: make(map[string]*ClientWorker, 12),
	}
}

func (s *Server) Heartbeats() map[string]*ClientWorker {
	return s.heartbeats
}

func (s *Server) Store() storage.Store {
	return s.store
}

func (s *Server) Start() error {
	store, err := storage.Open("rocksdb", s.Options.StoragePath)
	if err != nil {
		return err
	}
	defer store.Close()

	addr, err := net.ResolveTCPAddr("tcp", s.Options.Binding)
	if err != nil {
		return err
	}
	listener, err := net.ListenTCP("tcp", addr)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.store = store
	s.scheduler = s.StartScheduler()
	s.listener = listener
	s.mu.Unlock()

	defer s.scheduler.Stop()

	// wait for outstanding requests to finish
	defer s.pending.Wait()

	for _, x := range EventHandlers {
		x(s)
	}

	// this is the central runtime loop for the main goroutine
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return nil
		}
		go func() {
			s.pending.Add(1)
			defer s.pending.Done()

			s.processConnection(conn)
		}()
	}

	return nil
}

func (s *Server) Stop(f func()) {
	// Don't allow new network connections
	s.mu.Lock()
	if s.listener != nil {
		s.listener.Close()
	}
	s.mu.Unlock()
	time.Sleep(10 * time.Millisecond)

	if f != nil {
		f()
	}
}

func (s *Server) processConnection(conn net.Conn) {
	// AHOY operation must complete within 1 second
	conn.SetDeadline(time.Now().Add(1 * time.Second))

	buf := bufio.NewReader(conn)

	line, err := buf.ReadString('\n')
	if err != nil {
		util.Error("Closing connection", err, nil)
		conn.Close()
		return
	}

	valid := strings.HasPrefix(line, "AHOY {")
	if !valid {
		util.Info("Invalid preamble", line)
		util.Info("Need a valid AHOY")
		conn.Close()
		return
	}

	data := line[5:]
	var client ClientWorker
	err = json.Unmarshal([]byte(data), &client)
	if err != nil {
		util.Error("Invalid client data", err, nil)
		conn.Close()
		return
	}

	if s.Options.Password != "" && client.Password != s.Options.Password {
		util.Info("Invalid password")
		conn.Close()
		return
	}

	client.Password = "<secret>"
	util.Debugf("%+v", client)

	if client.Wid == "" {
		util.Error("Invalid client Wid", err, nil)
		conn.Close()
		return
	}

	val, ok := s.heartbeats[client.Wid]
	if ok {
		val.lastHeartbeat = time.Now()
	} else {
		s.heartbeats[client.Wid] = &client
		client.StartedAt = time.Now()
		client.lastHeartbeat = time.Now()
	}

	_, err = conn.Write([]byte("+OK\r\n"))
	if err != nil {
		util.Error("Closing connection", err, nil)
		conn.Close()
		return
	}

	// disable deadline
	conn.SetDeadline(time.Time{})

	c := &Connection{
		client: &client,
		ident:  conn.RemoteAddr().String(),
		conn:   conn,
		buf:    buf,
	}

	processLines(c, s)
}

type command func(c *Connection, s *Server, cmd string)

var cmdSet = map[string]command{
	"END":   end,
	"PUSH":  push,
	"POP":   pop,
	"ACK":   ack,
	"FAIL":  fail,
	"BEAT":  heartbeat,
	"INFO":  info,
	"STORE": store,
}

func end(c *Connection, s *Server, cmd string) {
	c.Close()
}

func push(c *Connection, s *Server, cmd string) {
	data := []byte(cmd[5:])
	job, err := parseJob(data)
	if err != nil {
		c.Error(cmd, err)
		return
	}

	if job.At != "" {
		t, err := util.ParseTime(job.At)
		if err != nil {
			c.Error(cmd, fmt.Errorf("Invalid timestamp for job.at: %s", job.At))
			return
		}

		if t.After(time.Now()) {
			data, err = json.Marshal(job)
			if err != nil {
				c.Error(cmd, err)
				return
			}
			// scheduler for later
			err = s.store.Scheduled().AddElement(job.At, job.Jid, data)
			if err != nil {
				c.Error(cmd, err)
				return
			}
			c.Ok()
			return
		}
	}

	// enqueue immediately
	q, err := s.store.GetQueue(job.Queue)
	if err != nil {
		c.Error(cmd, err)
		return
	}

	job.EnqueuedAt = util.Nows()
	data, err = json.Marshal(job)
	if err != nil {
		c.Error(cmd, err)
		return
	}

	err = q.Push(data)
	if err != nil {
		c.Error(cmd, err)
		return
	}

	c.Ok()
}

func pop(c *Connection, s *Server, cmd string) {
	qs := strings.Split(cmd, " ")[1:]
	job, err := s.Pop(func(job *faktory.Job) error {
		return s.Reserve(c.client.Wid, job)
	}, qs...)
	if err != nil {
		c.Error(cmd, err)
		return
	}
	if job != nil {
		res, err := json.Marshal(job)
		if err != nil {
			c.Error(cmd, err)
			return
		}
		atomic.AddInt64(&s.Processed, 1)
		c.Result(res)
	} else {
		c.Result(nil)
	}
}

func ack(c *Connection, s *Server, cmd string) {
	jid := cmd[4:]
	_, err := s.Acknowledge(jid)
	if err != nil {
		c.Error(cmd, err)
		return
	}

	c.Ok()
}

func info(c *Connection, s *Server, cmd string) {
	defalt, err := s.store.GetQueue("default")
	if err != nil {
		c.Error(cmd, err)
		return
	}
	data := map[string]interface{}{
		"failures":  s.Failures,
		"processed": s.Processed,
		"working":   s.scheduler.Working.Stats(),
		"retries":   s.scheduler.Retries.Stats(),
		"scheduled": s.scheduler.Scheduled.Stats(),
		"default":   defalt.Size(),
	}
	bytes, err := json.Marshal(data)
	if err != nil {
		c.Error(cmd, err)
		return
	}

	c.Result(bytes)
}

func store(c *Connection, s *Server, cmd string) {
	subcmd := strings.ToLower(strings.Split(cmd, " ")[1])
	switch subcmd {
	case "stats":
		c.Result([]byte(s.store.Stats()["stats"]))
	case "backup":
		// TODO
	default:
		c.Error(cmd, fmt.Errorf("Unknown STORE command: %s", subcmd))
	}
}

func processLines(conn *Connection, server *Server) {
	for {
		cmd, e := conn.buf.ReadString('\n')
		if e != nil {
			if e != io.EOF {
				util.Error("Unexpected socket error", e, nil)
			}
			conn.Close()
			return
		}
		cmd = strings.TrimSuffix(cmd, "\r\n")
		cmd = strings.TrimSuffix(cmd, "\n")
		//util.Debug(cmd)

		idx := strings.Index(cmd, " ")
		verb := cmd
		if idx >= 0 {
			verb = cmd[0:idx]
		}
		proc, ok := cmdSet[verb]
		if !ok {
			conn.Error(cmd, fmt.Errorf("Unknown command %s", verb))
		} else {
			proc(conn, server, cmd)
		}
		if verb == "END" {
			break
		}
	}
}

/*
BEAT {"wid":1238971623}
*/
func heartbeat(c *Connection, s *Server, cmd string) {
	if !strings.HasPrefix(cmd, "BEAT {") {
		c.Error(cmd, fmt.Errorf("Invalid format %s", cmd))
		return
	}

	var worker ClientWorker
	data := cmd[5:]
	err := json.Unmarshal([]byte(data), &worker)
	if err != nil {
		c.Error(cmd, fmt.Errorf("Invalid format %s", data))
		return
	}

	entry, ok := s.heartbeats[worker.Wid]
	if !ok {
		c.Error(cmd, fmt.Errorf("Unknown client %d", worker.Wid))
		return
	}

	entry.lastHeartbeat = time.Now()

	if entry.signal == "" {
		c.Ok()
	} else {
		c.Result([]byte(fmt.Sprintf(`{"signal":"%s"}`, entry.signal)))
	}
}

/*
 * Removes any heartbeat records over 1 minute old.
 */
func (s *Server) reapHeartbeats() {
	toDelete := []string{}

	for k, worker := range s.heartbeats {
		if worker.lastHeartbeat.Before(time.Now().Add(-1 * time.Minute)) {
			toDelete = append(toDelete, k)
		}
	}

	for _, k := range toDelete {
		delete(s.heartbeats, k)
	}
}

func parseJob(buf []byte) (*faktory.Job, error) {
	var job faktory.Job

	err := json.Unmarshal(buf, &job)
	if err != nil {
		return nil, err
	}

	if job.CreatedAt == "" {
		job.CreatedAt = util.Nows()
	}
	if job.Queue == "" {
		job.Queue = "default"
	}
	return &job, nil
}
