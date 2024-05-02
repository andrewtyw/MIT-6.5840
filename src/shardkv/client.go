package shardkv

//
// client code to talk to a sharded key/value service.
//
// the client first talks to the shardctrler to find out
// the assignment of shards (keys) to groups, and then
// talks to the group that holds the key's shard.
//

import (
	"6.5840/labrpc"
	"fmt"
	"sync"
)
import "crypto/rand"
import "math/big"
import "6.5840/shardctrler"
import "time"

// which shard is a key in?
// please use this function,
// and please do not change it.
func key2shard(key string) int {
	shard := 0
	if len(key) > 0 {
		shard = int(key[0])
	}
	shard %= shardctrler.NShards
	return shard
}

func nrand() int64 {
	maxInt := big.NewInt(int64(1) << 62)
	bigx, _ := rand.Int(rand.Reader, maxInt)
	x := bigx.Int64()
	return x
}

type Clerk struct {
	sm       *shardctrler.Clerk
	config   shardctrler.Config
	make_end func(string) *labrpc.ClientEnd
	// You will have to modify this struct.
	clientID  int64      // read only
	mutex     sync.Mutex // protects the following
	serialNum uint64     // each Get/Put/Append function call will be allocated unique serialNum
}

// Get fetch the current value for a key.
// returns "" if the key does not exist.
// keeps trying forever in the face of all other errors.
// You will have to modify this function.
func (ck *Clerk) Get(key string) string {
	args := GetArgs{}
	args.Key = key
	args.ClientID = ck.clientID
	ck.mutex.Lock()
	ck.serialNum++
	args.SerialNum = ck.serialNum
	ck.mutex.Unlock()

	for {
		shard := key2shard(key)
		gid := ck.config.Shards[shard]
		if servers, ok := ck.config.Groups[gid]; ok {
			// try each server for the shard.
			for si := 0; si < len(servers); si++ {
				srv := ck.make_end(servers[si])
				var reply GetReply
				ok := srv.Call("ShardKV.Get", &args, &reply)
				if ok && (reply.Err == OK || reply.Err == ErrNoKey) {
					return reply.Value
				}
				//log.Printf("err=%v ", reply.Err)
				if ok && (reply.Err == ErrWrongGroup) {
					// increment serialNum and retry
					ck.mutex.Lock()
					ck.serialNum++
					args.SerialNum = ck.serialNum
					ck.mutex.Unlock()
					break
				}
				if ok && (reply.Err == ErrShardNotReady) {
					// increment serialNum and retry
					ck.mutex.Lock()
					ck.serialNum++
					args.SerialNum = ck.serialNum
					ck.mutex.Unlock()
					break
				}
				// ... not ok, or ErrWrongLeader
			}
		}
		time.Sleep(100 * time.Millisecond)
		// ask controller for the latest configuration.
		//log.Printf("query cfg init ")
		ck.config = ck.sm.Query(-1)
		//log.Printf("query cfg done ")
	}

	return ""
}

// PutAppend shared by Put and Append.
// You will have to modify this function.
func (ck *Clerk) PutAppend(key string, value string, op string) {
	args := PutAppendArgs{}
	args.Key = key
	args.Value = value
	args.Op = op
	args.ClientID = ck.clientID
	ck.mutex.Lock()
	ck.serialNum++
	args.SerialNum = ck.serialNum
	ck.mutex.Unlock()
	nTry := 0
	for {
		shard := key2shard(key)
		gid := ck.config.Shards[shard]
		if servers, ok := ck.config.Groups[gid]; ok {
			for si := 0; si < len(servers); si++ {
				srv := ck.make_end(servers[si])
				var reply PutAppendReply
				ok := srv.Call("ShardKV.PutAppend", &args, &reply)
				nTry++
				if ok && reply.Err == OK {
					return
				}
				if ok && reply.Err == ErrRepeatedRequest {
					return // todo: prove
				}
				if ok && reply.Err == ErrConfigNotReady {
					break
				}
				if ok && (reply.Err == ErrWrongGroup) {
					// increment serialNum and retry
					ck.mutex.Lock()
					ck.serialNum++
					args.SerialNum = ck.serialNum
					ck.mutex.Unlock()
					break
				}
				if ok && (reply.Err == ErrShardNotReady) {
					// increment serialNum and retry
					fmt.Printf("key=%v not ready, retry\n", args.Key)
					ck.mutex.Lock()
					ck.serialNum++
					args.SerialNum = ck.serialNum
					ck.mutex.Unlock()
					break
				}
				// ... not ok, or ErrWrongLeader
			}
		}
		time.Sleep(300 * time.Millisecond)
		//fmt.Printf("ntry=%v\n", nTry)
		// ask controller for the latest configuration.
		ck.config = ck.sm.Query(-1)
	}
}

func (ck *Clerk) Put(key string, value string) {
	ck.PutAppend(key, value, PUT)
}
func (ck *Clerk) Append(key string, value string) {
	ck.PutAppend(key, value, APPEND)
}

// MakeClerk is called by the tester
//
// ctrlers[] is needed to call shardctrler.MakeClerk().
//
// make_end(servername) turns a server name from a
// Config.Groups[gid][i] into a labrpc.ClientEnd on which you can
// send RPCs.
func MakeClerk(ctrlers []*labrpc.ClientEnd, make_end func(string) *labrpc.ClientEnd) *Clerk {
	ck := new(Clerk)
	ck.sm = shardctrler.MakeClerk(ctrlers)
	ck.make_end = make_end
	// You'll have to add code here.
	ck.clientID = nrand()
	ck.serialNum = 0
	ck.mutex = sync.Mutex{}
	return ck
}
