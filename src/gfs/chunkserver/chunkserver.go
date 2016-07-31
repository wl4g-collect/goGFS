package chunkserver

import (
    "fmt"
	log "github.com/Sirupsen/logrus"
	//"math/rand"
	"net"
	"net/rpc"
	"os"
    "io"
	"path"
	"sync"
	"time"

	"gfs"
	"gfs/util"
)

// ChunkServer struct
type ChunkServer struct {
	address    gfs.ServerAddress // chunkserver address
	master     gfs.ServerAddress // master address
	serverRoot string            // path to data storage
	l          net.Listener
	shutdown   chan struct {}

	dl                *downloadBuffer               // expiring download buffer
	pendingLeaseExtensions *util.ArraySet           // pending lease extension
	chunk             map[gfs.ChunkHandle]*chunkInfo // chunk information
    dead              bool    // set to ture if server is shuntdown
}

type Mutation struct {
    mtype   gfs.MutationType
    version gfs.ChunkVersion
    data    []byte
    offset  gfs.Offset
}

type chunkInfo struct {
	sync.RWMutex
	length        gfs.Offset
    version       gfs.ChunkVersion  // version number of the chunk in disk 
    newestVersion gfs.ChunkVersion  // allocated newest version number
    mutations     map[gfs.ChunkVersion]*Mutation // mutation buffer
}

// return the next version for chunk
func (ck *chunkInfo) nextVersion() gfs.ChunkVersion {
    ck.newestVersion++
    return ck.newestVersion
}

// NewAndServe starts a chunkserver and return the pointer to it.
func NewAndServe(addr, masterAddr gfs.ServerAddress, serverRoot string) *ChunkServer {
	cs := &ChunkServer{
		address:           addr,
        shutdown:          make(chan struct{}),
		master:            masterAddr,
		serverRoot:        serverRoot,
		dl:                newDownloadBuffer(gfs.DownloadBufferExpire, gfs.DownloadBufferTick),
		pendingLeaseExtensions: new(util.ArraySet),
        chunk:             make(map[gfs.ChunkHandle]*chunkInfo),
	}
	rpcs := rpc.NewServer()
	rpcs.Register(cs)
	l, e := net.Listen("tcp", string(cs.address))
	if e != nil {
		log.Fatal("listen error:", e)
		log.Exit(1)
	}
	cs.l = l

    // Mkdir
    err := os.Mkdir(serverRoot, 0744)
    if err != nil { log.Fatal("mkdir", err) }

	shutdown := make(chan struct{})
	// RPC Handler
	go func() {
	loop:
		for {
			select {
			case <-cs.shutdown:
                close(shutdown)
				break loop
			default:
			}
            conn, err := cs.l.Accept()
            if err == nil {
                go func() {
                    rpcs.ServeConn(conn)
                    conn.Close()
                }()
            } else {
                //log.Fatal("accept error:", err)
            }
		}
	}()

	// Heartbeat
	go func() {
	loop:
		for {
			select {
			case <-shutdown:
				break loop
			default:
			}
			pe := cs.pendingLeaseExtensions.GetAllAndClear()
			le := make([]gfs.ChunkHandle, len(pe))
			for i, v := range pe {
				le[i] = v.(gfs.ChunkHandle)
			}
			args := &gfs.HeartbeatArg{
				Address:         addr,
				LeaseExtensions: le,
			}
			if err := util.Call(cs.master, "Master.RPCHeartbeat", args, nil); err != nil {
				log.Fatal("heartbeat rpc error ", err)
				log.Exit(1)
			}

			time.Sleep(gfs.HeartbeatInterval)
		}
	}()

	log.Infof("ChunkServer is now running. addr = %v, root path = %v, master addr = %v", addr, serverRoot, masterAddr)

	return cs
}

// Shutdown shuts the chunkserver down
func (cs *ChunkServer) Shutdown() {
    log.Warning(cs.address, " Shutdown")
    close(cs.shutdown)
    cs.l.Close()
    cs.dead = true
}

// RPCPushDataAndForward is called by client.
// It saves client pushed data to memory buffer and forward to all other replicas.
// Returns a DataID which represents the index in the memory buffer.
func (cs *ChunkServer) RPCPushDataAndForward(args gfs.PushDataAndForwardArg, reply *gfs.PushDataAndForwardReply) error {
	if len(args.Data) > gfs.MaxChunkSize {
		return fmt.Errorf("Data is too large. Size %v > MaxSize %v", len(args.Data), gfs.MaxChunkSize)
	}

    id := cs.dl.New(args.Handle)
    cs.dl.Set(id, args.Data)
    //log.Infof("Server %v : get data %v (primary)", cs.address, id)

    err := util.CallAll(args.ForwardTo, "ChunkServer.RPCForwardData", gfs.ForwardDataArg{id, args.Data})

    reply.DataID = id
    return err
}

// RPCForwardData is called by another replica who sends data to the current memory buffer.
// TODO: This should be replaced by a chain forwarding.
func (cs *ChunkServer) RPCForwardData(args gfs.ForwardDataArg, reply *gfs.ForwardDataReply) error {
    if _, ok := cs.dl.Get(args.DataID); ok {
        return fmt.Errorf("Data %v already exists", args.DataID)
    }

    //log.Infof("Server %v : get data %v", cs.address, args.DataID)
    cs.dl.Set(args.DataID, args.Data)
    return nil
}

// RPCCreateChunk is called by master to create a new chunk given the chunk handle.
func (cs *ChunkServer) RPCCreateChunk(args gfs.CreateChunkArg, reply *gfs.CreateChunkReply) error {
    log.Infof("%v create chunk %v", cs.address, args.Handle)

    if _, ok := cs.chunk[args.Handle]; ok {
        log.Warning("an ignored error on RPCCreateChunk")
        return nil // TODO : error handle
        //return fmt.Errorf("Chunk %v already exists", args.Handle)
    }

    cs.chunk[args.Handle] = &chunkInfo{
        length:    0,
        mutations: make(map[gfs.ChunkVersion]*Mutation),
    }
    filename := path.Join(cs.serverRoot, fmt.Sprintf("chunk%v.chk", args.Handle))
    _, err := os.OpenFile(filename, os.O_WRONLY | os.O_CREATE, 0744)
    if err != nil { return err }
    return nil
}

// RPCReadChunk is called by client, read chunk data and return
func (cs *ChunkServer) RPCReadChunk(args gfs.ReadChunkArg, reply *gfs.ReadChunkReply) error {
    handle := args.Handle
    ck, ok := cs.chunk[handle]
    if !ok { return fmt.Errorf("Cannot find chunk %v", handle) }

    // read from disk
    var err error

    reply.Data = make([]byte, args.Length)
    ck.RLock()
    reply.Length, err = cs.readChunk(handle, args.Offset, reply.Data)
    ck.RUnlock()
    if err == io.EOF {
        reply.ErrorCode = gfs.ReadEOF
        return nil
    }

    if err != nil { return err }
    return nil
}

// RPCWriteChunk is called by client
// applies chunk write to itself (primary) and asks secondaries to do the same.
func (cs *ChunkServer) RPCWriteChunk(args gfs.WriteChunkArg, reply *gfs.WriteChunkReply) error {
    data, err := cs.deleteDownloadedData(args.DataID)
    if err != nil { return err }

    newLen := args.Offset + gfs.Offset(len(data))
    if newLen > gfs.MaxChunkSize {
		return fmt.Errorf("writeChunk new length is too large. Size %v > MaxSize %v", len(data), gfs.MaxChunkSize)
    }

    handle := args.DataID.Handle
    ck, ok := cs.chunk[handle]
    if !ok { return fmt.Errorf("Cannot find chunk %v", handle) }

    ck.Lock()
    // assign a new version
    if (newLen > ck.length) {
        ck.length = newLen
    }
    version := ck.nextVersion()
    if _, ok := ck.mutations[version]; ok {
        return fmt.Errorf("3 Duplicated mutation version %v for chunk %v", version, handle)
    }
    ck.mutations[version] = &Mutation{gfs.MutationWrite, version, data, args.Offset}
    ck.Unlock()

    // apply to local
    err = cs.doMutation(handle)
    if err != nil { return err }

    // call secondaries
    callArgs := gfs.ApplyMutationArg{gfs.MutationWrite, version, args.DataID, args.Offset}
    err = util.CallAll(args.Secondaries, "ChunkServer.RPCApplyMutation", callArgs);
    if err != nil { return err }


    // extend lease
    cs.pendingLeaseExtensions.Add(handle)

    return nil
}

// RPCAppendChunk is called by client to apply atomic record append.
// The length of data should be within 1/4 chunk size.
// If the chunk size after appending the data will excceed the limit,
// pad current chunk and ask the client to retry on the next chunk.
func (cs *ChunkServer) RPCAppendChunk(args gfs.AppendChunkArg, reply *gfs.AppendChunkReply) error {
	data, err := cs.deleteDownloadedData(args.DataID)
	if err != nil { return err }

	if len(data) > gfs.MaxAppendSize {
		return fmt.Errorf("Append data size %v excceeds max append size %v", len(data), gfs.MaxAppendSize)
	}

    handle := args.DataID.Handle
	ck, ok := cs.chunk[handle]
	if !ok { return fmt.Errorf("cannot find chunk %v", handle) }

    var mtype gfs.MutationType

    ck.Lock()
	newLen := ck.length + gfs.Offset(len(data))
	offset := ck.length
	if newLen > gfs.MaxChunkSize {
        mtype = gfs.MutationPad
        ck.length = gfs.MaxChunkSize
        reply.ErrorCode  = gfs.AppendExceedChunkSize
    } else {
        mtype = gfs.MutationAppend
        ck.length = newLen
    }
	reply.Offset = offset

    // allocate a new version
    version := ck.nextVersion()
    if _, ok := ck.mutations[version]; ok {
        log.Warning(ck.mutations)
        return fmt.Errorf("1 Duplicated mutation version %v for chunk %v", version, handle)
    }
    ck.mutations[version] = &Mutation{mtype, version, data, offset}
    ck.Unlock()

    log.Infof("Primary %v : append chunk %v version %v", cs.address, args.DataID.Handle, version)

    // apply to local
    err = cs.doMutation(handle)
    if err != nil { return err }

    // call secondaries
    callArgs := gfs.ApplyMutationArg{mtype, version, args.DataID, offset}
    err = util.CallAll(args.Secondaries, "ChunkServer.RPCApplyMutation", callArgs);
    if err != nil { return err }

    // extend lease
    cs.pendingLeaseExtensions.Add(handle)

    return nil
}

// RPCApplyWriteChunk is called by primary to apply mutations
func (cs *ChunkServer) RPCApplyMutation(args gfs.ApplyMutationArg, reply *gfs.ApplyMutationReply) error {
    data, err := cs.deleteDownloadedData(args.DataID)
    if err != nil { return err }

    handle := args.DataID.Handle
    ck, ok := cs.chunk[handle]
    if !ok { return fmt.Errorf("Cannot find chunk %v", handle) }


    mutation := Mutation{args.Mtype, args.Version, data, args.Offset}
    ck.Lock()
    if _, ok := ck.mutations[args.Version]; ok {
        return fmt.Errorf("2 Duplicated mutation version %v for chunk %v", args.Version, handle)
    }
    ck.mutations[args.Version] = &mutation
    ck.Unlock()

    log.Infof("Server %v : get mutation to chunk %v version %v", cs.address, handle, args.Version)

    err = cs.doMutation(handle)
    return err
}

// RPCSendCCopy is called by master, send the whole copy to given address
func (cs *ChunkServer) RPCSendCopy(args gfs.SendCopyArg, reply *gfs.SendCopyReply) error {
    handle := args.Handle
    ck, ok := cs.chunk[handle]
    if !ok { return fmt.Errorf("Cannot find chunk %v", handle) }

    ck.Lock()
    defer ck.Unlock()

    log.Infof("%v : Send copy of %v to %v", cs.address, handle, args.Address)
    data := make([]byte, ck.length)
    _, err := cs.readChunk(handle, 0, data)
    if err != nil { return err }

    if ck.version != ck.newestVersion {
        return fmt.Errorf("chunk %v In mutation", handle)
        //reply.ErrorCode = gfs.NotAvailableForCopy
    }

    var r gfs.ApplyCopyReply
    err = util.Call(args.Address, "ChunkServer.RPCApplyCopy", gfs.ApplyCopyArg{handle, data, ck.version}, &r)
    if err != nil { return err }

    return nil
}

// RPCSendCCopy is called by another replica
// rewrite the local version to given copy data
func (cs *ChunkServer) RPCApplyCopy(args gfs.ApplyCopyArg, reply *gfs.ApplyCopyReply) error {
    handle := args.Handle
    ck, ok := cs.chunk[handle]
    if !ok { return fmt.Errorf("Cannot find chunk %v", handle) }

    ck.Lock()
    defer ck.Unlock()

    log.Infof("%v : Apply copy of %v", cs.address, handle)

    ck.mutations = make(map[gfs.ChunkVersion]*Mutation, 0) // clear mutation buffer
    err := cs.writeChunk(handle, args.Version, args.Data, 0, true)
    if err != nil { return err }
    return nil
}

// deleteDownloadedData returns the corresponding data and delete it from the buffer.
func (cs *ChunkServer) deleteDownloadedData(id gfs.DataBufferID) ([]byte, error) {
	data, ok := cs.dl.Get(id)
	if !ok {
		return nil, fmt.Errorf("DataID %v not found in download buffer.", id)
	}
	cs.dl.Delete(id)

	return data, nil
}

// writeChunk writes data at offset to a chunk at disk
func (cs *ChunkServer) writeChunk(handle gfs.ChunkHandle, version gfs.ChunkVersion, data []byte, offset gfs.Offset, lock bool) error {
    ck := cs.chunk[handle]

    newLen := offset + gfs.Offset(len(data))
    if newLen > ck.length {
        ck.length = newLen
    }
    ck.version = version

    log.Infof("Server %v : write to chunk %v version %v", cs.address, handle, version)
    filename := path.Join(cs.serverRoot, fmt.Sprintf("chunk%v.chk", handle))
    file, err := os.OpenFile(filename, os.O_WRONLY | os.O_CREATE, 0744)
    if err != nil { return err }
    defer file.Close()

     _, err = file.WriteAt(data, int64(offset));
    if err != nil { return err }

    return nil
}

// readChunk reads data at offset from a chunk at dist
func (cs *ChunkServer) readChunk(handle gfs.ChunkHandle, offset gfs.Offset, data []byte) (int, error){
    filename := path.Join(cs.serverRoot, fmt.Sprintf("chunk%v.chk", handle))

    f, err := os.Open(filename)
    if err != nil { return -1, err }
    defer f.Close()  // f.Close will run when we're finished.

    return f.ReadAt(data, int64(offset))
}

// apply mutations (write, append, pas) in chunk buffer in proper order according to version number
func (cs *ChunkServer) doMutation(handle gfs.ChunkHandle) error {
	ck := cs.chunk[handle]

    ck.Lock()
    defer ck.Unlock()
    for {
        v, ok := ck.mutations[ck.version + 1]

        if ok {
            var lock bool
            if v.mtype == gfs.MutationAppend {
                lock = true
            } else {
                lock = false
            }

            var err error
            if v.mtype == gfs.MutationPad {
                //cs.padChunk(handle, v.version)
                data := []byte{0}
                err = cs.writeChunk(handle, v.version, data, gfs.MaxChunkSize - 1, lock)
            } else {
                err = cs.writeChunk(handle, v.version, v.data, v.offset, lock)
            }

            delete(ck.mutations, v.version)

            if err != nil { return err }
        } else {
            break
        }
    }

    // TODO : detect older version mutation

    if len(ck.mutations) == 0 {
        ck.newestVersion = ck.version
    }

    return nil
}

// padChunk pads a chunk to max chunk size.
// <code>cs.chunk[handle]</code> should be locked in advance
func (cs *ChunkServer) padChunk(handle gfs.ChunkHandle, version gfs.ChunkVersion) error {
    ck := cs.chunk[handle]
    ck.version = version
    ck.length = gfs.MaxChunkSize

    return nil
}

// =================== DEBUG TOOLS =================== 
func getContents(filename string) (string, error) {
    f, err := os.Open(filename)
    if err != nil {
        return "", err
    }
    defer f.Close()  // f.Close will run when we're finished.

    var result []byte
    buf := make([]byte, 100)
    for {
        n, err := f.Read(buf[0:])
        if err != nil {
            if err == io.EOF {
                break
            }
            return "", err  // f will be closed if we return here.
        }
        result = append(result, buf[0:n]...) // append is discussed later.
    }
    return string(result), nil // f will be closed if we return here.
}

func (cs *ChunkServer) PrintSelf() error {
    log.Info("============ ", cs.address, " ============")
    if cs.dead {
        log.Warning("DEAD")
    } else {
        for h, v := range cs.chunk {
            filename := path.Join(cs.serverRoot, fmt.Sprintf("chunk%v.chk", h))
            log.Infof("chunk %v : version %v", h, v.version)
            str, _ := getContents(filename)
            log.Info(str)
        }
    }
    return nil
}