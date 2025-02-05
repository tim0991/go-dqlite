package bindings

import "C"
import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
	"unsafe"

	"github.com/canonical/go-dqlite/internal/protocol"
)

type Node C.dqlite_node

type SnapshotParams struct {
	Threshold uint64
	Trailing  uint64
}

// Initializes state.
func init() {
	// FIXME: ignore SIGPIPE, see https://github.com/joyent/libuv/issues/1254
	C.signal(C.SIGPIPE, C.SIG_IGN)
}

func ConfigSingleThread() error {
	if rc := C.sqlite3ConfigSingleThread(); rc != 0 {
		return protocol.Error{Message: C.GoString(C.sqlite3_errstr(rc)), Code: int(rc)}
	}
	return nil
}

func ConfigMultiThread() error {
	if rc := C.sqlite3ConfigMultiThread(); rc != 0 {
		return protocol.Error{Message: C.GoString(C.sqlite3_errstr(rc)), Code: int(rc)}
	}
	return nil
}

// NewNode creates a new Node instance.
func NewNode(id uint64, address string, dir string) (*Node, error) {
	var server *C.dqlite_node
	cid := C.dqlite_node_id(id)

	caddress := C.CString(address)
	defer C.free(unsafe.Pointer(caddress))

	cdir := C.CString(dir)
	defer C.free(unsafe.Pointer(cdir))

	if rc := C.dqlite_node_create(cid, caddress, cdir, &server); rc != 0 {
		errmsg := C.GoString(C.dqlite_node_errmsg(server))
		return nil, fmt.Errorf("%s", errmsg)
	}

	return (*Node)(unsafe.Pointer(server)), nil
}

func (s *Node) SetDialFunc(dial protocol.DialFunc) error {
	server := (*C.dqlite_node)(unsafe.Pointer(s))
	connectLock.Lock()
	defer connectLock.Unlock()
	connectIndex++
	connectRegistry[connectIndex] = dial
	if rc := C.configConnectFunc(server, connectIndex); rc != 0 {
		return fmt.Errorf("failed to set connect func")
	}
	return nil
}

func (s *Node) SetBindAddress(address string) error {
	server := (*C.dqlite_node)(unsafe.Pointer(s))
	caddress := C.CString(address)
	defer C.free(unsafe.Pointer(caddress))
	if rc := C.dqlite_node_set_bind_address(server, caddress); rc != 0 {
		return fmt.Errorf("failed to set bind address %q: %d", address, rc)
	}
	return nil
}

func (s *Node) SetNetworkLatency(nanoseconds uint64) error {
	server := (*C.dqlite_node)(unsafe.Pointer(s))
	cnanoseconds := C.nanoseconds_t(nanoseconds)
	if rc := C.dqlite_node_set_network_latency(server, cnanoseconds); rc != 0 {
		return fmt.Errorf("failed to set network latency")
	}
	return nil
}

func (s *Node) SetSnapshotParams(params SnapshotParams) error {
	server := (*C.dqlite_node)(unsafe.Pointer(s))
	cthreshold := C.unsigned(params.Threshold)
	ctrailing := C.unsigned(params.Trailing)
	if rc := C.dqlite_node_set_snapshot_params(server, cthreshold, ctrailing); rc != 0 {
		return fmt.Errorf("failed to set snapshot params")
	}
	return nil
}

func (s *Node) SetFailureDomain(code uint64) error {
	server := (*C.dqlite_node)(unsafe.Pointer(s))
	ccode := C.failure_domain_t(code)
	if rc := C.dqlite_node_set_failure_domain(server, ccode); rc != 0 {
		return fmt.Errorf("set failure domain: %d", rc)
	}
	return nil
}

func (s *Node) GetBindAddress() string {
	server := (*C.dqlite_node)(unsafe.Pointer(s))
	return C.GoString(C.dqlite_node_get_bind_address(server))
}

func (s *Node) Start() error {
	server := (*C.dqlite_node)(unsafe.Pointer(s))
	if rc := C.dqlite_node_start(server); rc != 0 {
		errmsg := C.GoString(C.dqlite_node_errmsg(server))
		return fmt.Errorf("%s", errmsg)
	}
	return nil
}

func (s *Node) Stop() error {
	server := (*C.dqlite_node)(unsafe.Pointer(s))
	if rc := C.dqlite_node_stop(server); rc != 0 {
		return fmt.Errorf("task stopped with error code %d", rc)
	}
	return nil
}

// Close the server releasing all used resources.
func (s *Node) Close() {
	server := (*C.dqlite_node)(unsafe.Pointer(s))
	C.dqlite_node_destroy(server)
}

// Remark that Recover doesn't take the node role into account
func (s *Node) Recover(cluster []protocol.NodeInfo) error {
	for i, _ := range cluster {
		cluster[i].Role = protocol.Voter
	}
	return s.RecoverExt(cluster)
}

// RecoverExt has a similar purpose as `Recover` but takes the node role into account
func (s *Node) RecoverExt(cluster []protocol.NodeInfo) error {
	server := (*C.dqlite_node)(unsafe.Pointer(s))
	n := C.int(len(cluster))
	infos := C.makeInfos(n)
	defer C.free(unsafe.Pointer(infos))
	for i, info := range cluster {
		cid := C.dqlite_node_id(info.ID)
		caddress := C.CString(info.Address)
		crole := C.int(info.Role)
		defer C.free(unsafe.Pointer(caddress))
		C.setInfo(infos, C.unsigned(i), cid, caddress, crole)
	}
	if rc := C.dqlite_node_recover_ext(server, infos, n); rc != 0 {
		return fmt.Errorf("recover failed with error code %d", rc)
	}
	return nil
}

// GenerateID generates a unique ID for a server.
func GenerateID(address string) uint64 {
	caddress := C.CString(address)
	defer C.free(unsafe.Pointer(caddress))
	id := C.dqlite_generate_node_id(caddress)
	return uint64(id)
}

// Extract the underlying socket from a connection.
func connToSocket(conn net.Conn) (C.int, error) {
	file, err := conn.(fileConn).File()
	if err != nil {
		return C.int(-1), err
	}

	fd1 := C.int(file.Fd())

	// Duplicate the file descriptor, in order to prevent Go's finalizer to
	// close it.
	fd2 := C.dupCloexec(fd1)
	if fd2 < 0 {
		return C.int(-1), fmt.Errorf("failed to dup socket fd")
	}

	conn.Close()

	return fd2, nil
}

// Interface that net.Conn must implement in order to extract the underlying
// file descriptor.
type fileConn interface {
	File() (*os.File, error)
}

//export connectWithDial
func connectWithDial(handle C.uintptr_t, address *C.char, fd *C.int) C.int {
	connectLock.Lock()
	defer connectLock.Unlock()
	dial := connectRegistry[handle]
	// TODO: make timeout customizable.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dial(ctx, C.GoString(address))
	if err != nil {
		return C.RAFT_NOCONNECTION
	}
	socket, err := connToSocket(conn)
	if err != nil {
		return C.RAFT_NOCONNECTION
	}
	*fd = socket
	return C.int(0)
}

// Use handles to avoid passing Go pointers to C.
var connectRegistry = make(map[C.uintptr_t]protocol.DialFunc)
var connectIndex C.uintptr_t = 100
var connectLock = sync.Mutex{}

// ErrNodeStopped is returned by Node.Handle() is the server was stopped.
var ErrNodeStopped = fmt.Errorf("server was stopped")

// To compare bool values.
var cfalse C.bool
