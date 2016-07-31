package client

import (
	"fmt"
    "io"
    //"time"
    "math/rand"

	log "github.com/Sirupsen/logrus"
	"gfs"
	"gfs/util"
)

// Client struct is the GFS client-side driver
type Client struct {
	master gfs.ServerAddress
}

// NewClient returns a new gfs client.
func NewClient(master gfs.ServerAddress) *Client {
	return &Client{
		master: master,
	}
}

// Create is a client API, creates a file
func (c *Client) Create(path gfs.Path) error {
    var reply gfs.CreateFileReply
    err := util.Call(c.master, "Master.RPCCreateFile", gfs.CreateFileArg{path}, &reply)
    if err != nil { return err }
    return nil
}

// Create is a client API, deletes a file
func (c *Client) Delete(path gfs.Path) error {
    var reply gfs.DeleteFileReply
    err := util.Call(c.master, "Master.RPCDeleteFile", gfs.DeleteFileArg{path}, &reply)
    if err != nil { return err }
    return nil
}

// Mkdir is a client API, makes a directory
func (c *Client) Mkdir(path gfs.Path) error {
    var reply gfs.MkdirReply
    err := util.Call(c.master, "Master.RPCMkdir", gfs.MkdirArg{path}, &reply)
    if err != nil { return err }
    return nil
}

// List is a client API, lists all files in specific directory
func (c *Client) List(path gfs.Path) ([]gfs.PathInfo, error) {
    var reply gfs.ListReply
    err := util.Call(c.master, "Master.RPCList", gfs.ListArg{path}, &reply)
    if err != nil { return nil, err }
    return reply.Files, nil
}

// Read is a client API, read file at specific offset
// it reads up to len(data) bytes form the File. it return the number of bytes and an error.
// the error is set to io.EOF if stream meets the end of file
func (c *Client) Read(path gfs.Path, offset gfs.Offset, data []byte) (n int, err error) {
    var f gfs.GetFileInfoReply
    err = util.Call(c.master, "Master.RPCGetFileInfo", gfs.GetFileInfoArg{path}, &f)
    if err != nil { return -1, err }

    if int64(offset / gfs.MaxChunkSize) > f.Chunks {
        return -1, fmt.Errorf("read offset exceeds file size")
    }

    pos := 0
    for {
        index := gfs.ChunkIndex(offset / gfs.MaxChunkSize)
        chunkOffset := offset % gfs.MaxChunkSize

        var handle gfs.ChunkHandle
        handle, err = c.GetChunkHandle(path, index)
        if err != nil { return }

        var n int
        for { // infinite try
            n, err = c.ReadChunk(handle, chunkOffset, data[pos:])
            if err == nil || err.(gfs.Error).Code == gfs.ReadEOF {
                break
            }
            log.Warning("Read connection error, try again: ", err)
        }

        offset += gfs.Offset(n)
        pos    += n
        if err != nil { break }

        if pos == len(data) { break }
    }

    if err != nil && err.(gfs.Error).Code == gfs.ReadEOF {
        return pos, io.EOF
    } else {
        return pos, err
    }
}

// Write is a client API. write data to file at specific offset
func (c *Client) Write(path gfs.Path, offset gfs.Offset, data []byte) error {
    begin := 0

    var f gfs.GetFileInfoReply
    err := util.Call(c.master, "Master.RPCGetFileInfo", gfs.GetFileInfoArg{path}, &f)
    if err != nil { return err }

    if int64(offset / gfs.MaxChunkSize) > f.Chunks {
        return fmt.Errorf("write offset exceeds file size")
    }

    for {
        index := gfs.ChunkIndex(offset / gfs.MaxChunkSize)
        chunkOffset := offset % gfs.MaxChunkSize

        handle, err := c.GetChunkHandle(path, index)
        if err != nil { return err }

        writeMax := int(gfs.MaxChunkSize - chunkOffset)
        var writeLen int
        if begin + writeMax > len(data) {
            writeLen = len(data) - begin
        } else {
            writeLen = writeMax
        }

        for {
            err = c.WriteChunk(handle, chunkOffset, data[begin : begin + writeLen])
            if err == nil { break }

            //log.Warning("Write : connection error, try again ", err)
        }
        if err != nil { return err }

        offset += gfs.Offset(writeLen)
        begin  += writeLen

        if begin == len(data) { break }
    }

    return nil
}

// Append is a client API, append data to file
func (c *Client) Append(path gfs.Path, data []byte) (offset gfs.Offset, err error) {
	if len(data) > gfs.MaxAppendSize {
		return 0, fmt.Errorf("len(data) = %v > max append size %v", len(data), gfs.MaxAppendSize)
	}

    var f gfs.GetFileInfoReply
    err = util.Call(c.master, "Master.RPCGetFileInfo", gfs.GetFileInfoArg{path}, &f)
    if err != nil { return }

    start := gfs.ChunkIndex(f.Chunks - 1)
    if (start < 0) { start = 0 }

    var chunkOffset gfs.Offset
    for {
        var handle gfs.ChunkHandle
        handle, err = c.GetChunkHandle(path, start)
        if err != nil { return }

        for {
            chunkOffset, err = c.AppendChunk(handle, data)
            if err == nil || err.(gfs.Error).Code == gfs.AppendExceedChunkSize{
                break
            }
            log.Warning("Append connection error, try again")
        }
        if err == nil || err.(gfs.Error).Code != gfs.AppendExceedChunkSize {
            break
        }

        // retry in next chunk
        start++
        log.Warning("pad this, try on next chunk ", start )
    }

    if err != nil { return }

    offset = gfs.Offset(start) * gfs.MaxChunkSize + chunkOffset
    return
}

// GetChunkHandle returns the chunk handle of (path, index).
// If the chunk doesn't exist, master will create one.
func (c *Client) GetChunkHandle(path gfs.Path, index gfs.ChunkIndex) (gfs.ChunkHandle, error) {
	var reply gfs.GetChunkHandleReply
	err := util.Call(c.master, "Master.RPCGetChunkHandle", gfs.GetChunkHandleArg{path, index}, &reply)
	if err != nil { return 0, err
	}
	return reply.Handle, nil
}

// ReadChunk read data from the chunk at specific offset.
// <code>len(data)+offset</data> should be within chunk size.
func (c *Client) ReadChunk(handle gfs.ChunkHandle, offset gfs.Offset, data []byte) (int, error) {
    var readLen int

    if gfs.MaxChunkSize - offset > gfs.Offset(len(data)) {
        readLen = len(data)
    } else {
        readLen = int(gfs.MaxChunkSize - offset)
    }

	var l gfs.GetReplicasReply
	err := util.Call(c.master, "Master.RPCGetReplicas", gfs.GetReplicasArg{handle}, &l)
	if err != nil { return 0, gfs.Error{gfs.UnknownError, err.Error()} }
    loc := l.Locations[rand.Int() % len(l.Locations)]

	var r gfs.ReadChunkReply
    r.Data = data

	err = util.Call(loc, "ChunkServer.RPCReadChunk", gfs.ReadChunkArg{handle, offset, readLen}, &r)
	if err != nil { return 0, gfs.Error{gfs.UnknownError, err.Error()} }
    if r.ErrorCode == gfs.ReadEOF {
        return r.Length, gfs.Error{gfs.ReadEOF, "read EOF"}
    }
    return r.Length, nil
}

// WriteChunk writes data to the chunk at specific offset.
// <code>len(data)+offset</data> should be within chunk size.
func (c *Client) WriteChunk(handle gfs.ChunkHandle, offset gfs.Offset, data []byte) error {
	if len(data)+int(offset) > gfs.MaxChunkSize {
		return fmt.Errorf("len(data)+offset = %v > max chunk size %v", len(data)+int(offset), gfs.MaxChunkSize)
	}
	var l gfs.GetPrimaryAndSecondariesReply
	err := util.Call(c.master, "Master.RPCGetPrimaryAndSecondaries", gfs.GetPrimaryAndSecondariesArg{handle}, &l)
	if err != nil { return err }

	var d gfs.PushDataAndForwardReply
	err = util.Call(l.Primary, "ChunkServer.RPCPushDataAndForward", gfs.PushDataAndForwardArg{handle, data, l.Secondaries}, &d)
	if err != nil { return err }

	wcargs := gfs.WriteChunkArg{d.DataID, offset, l.Secondaries}
	err = util.Call(l.Primary, "ChunkServer.RPCWriteChunk", wcargs, &gfs.WriteChunkReply{})
	return err
}

// AppendChunk appends data to a chunk.
// Chunk offset of the start of data will be returned if success.
// <code>len(data)</code> should be within 1/4 chunk size.
func (c *Client) AppendChunk(handle gfs.ChunkHandle, data []byte) (offset gfs.Offset, err error) {
	if len(data) > gfs.MaxAppendSize {
		return 0, gfs.Error{gfs.UnknownError, fmt.Sprintf("len(data) = %v > max append size %v", len(data), gfs.MaxAppendSize)}
	}

	var l gfs.GetPrimaryAndSecondariesReply
	err = util.Call(c.master, "Master.RPCGetPrimaryAndSecondaries", gfs.GetPrimaryAndSecondariesArg{handle}, &l)
	if err != nil {
        return -1, gfs.Error{gfs.UnknownError, err.Error()}
    }

    log.Infof("Client : push data to primary %v", data[:2])
	var d gfs.PushDataAndForwardReply
	err = util.Call(l.Primary, "ChunkServer.RPCPushDataAndForward", gfs.PushDataAndForwardArg{handle, data, l.Secondaries}, &d)
	if err != nil {
        return -1, gfs.Error{gfs.UnknownError, err.Error()}
    }

    log.Infof("Client : send append request to primary. data : %v", d.DataID)
	var a gfs.AppendChunkReply
	acargs := gfs.AppendChunkArg{d.DataID, l.Secondaries}
	err = util.Call(l.Primary, "ChunkServer.RPCAppendChunk", acargs, &a)
    if err != nil {
        return -1, gfs.Error{gfs.UnknownError, err.Error()}
    }
    if a.ErrorCode == gfs.AppendExceedChunkSize {
        return a.Offset, gfs.Error{a.ErrorCode, "append over chunks"}
    }
	return a.Offset, nil
}