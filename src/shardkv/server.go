package shardkv

import (
	"6.5840/labrpc"
	"6.5840/shardctrler"
	"fmt"
	"sync/atomic"
	"time"
)
import "6.5840/raft"
import "sync"
import "6.5840/labgob"

const requestTimeOut = 500 * time.Millisecond

type Shard struct {
	Version int               // determined by Config.Num
	Data    map[string]string //! can be nil?
}

func MakeSubDB(version int) Shard {
	return Shard{
		Version: version,
		Data:    make(map[string]string),
	}
}

type ShardKV struct {
	mu           sync.Mutex // protects the followings till cfgMutex
	me           int
	rf           *raft.Raft
	applyCh      chan raft.ApplyMsg
	make_end     func(string) *labrpc.ClientEnd
	gid          int
	ctrlers      []*labrpc.ClientEnd
	maxraftstate int // snapshot if log grows this big

	// Your definitions here.
	persister   *raft.Persister
	dead        int32
	scc         *shardctrler.Clerk // shard controller clerk. TODO: use for what?
	opDoneChans map[int]chan Op    // used for notify an Op is done
	// persist

	clientId2SerialNum map[int64]uint64           // to prevent duplicate requests
	inMemoryDB         [shardctrler.NShards]Shard // shardId -> DB
	currCfg            shardctrler.Config         // the latest shardctrler.Config; the server polls shardctrler for it periodically
	prevCfg            shardctrler.Config         // the previous config
	clientId           int64                      // when calling InstallShardData RPC, this machine is a client
	lastApplied        int                        // the index of latest applied log

	cfgPollingTicker    *time.Ticker // periodically send request to poll latest Config
	cfgPollingTimeoutMS int64        // config polling timeout
}

func db2str(db [shardctrler.NShards]Shard) string {
	res := " "
	for shardId, subDB := range db {
		res += fmt.Sprintf("%vv%vl%v; ", shardId, subDB.Version, len(subDB.Data))
	}
	return res
}

func (kv *ShardKV) deleteKeyFromOpDoneChans(cmdIdx int) {
	kv.mu.Lock()
	delete(kv.opDoneChans, cmdIdx)
	kv.mu.Unlock()
}

// ! should be used in cfgMutex context
func (kv *ShardKV) isCfgResponsibleForKey(cfg shardctrler.Config, key string) bool {
	sid := key2shard(key)
	expectGID := cfg.Shards[sid]
	return kv.gid == expectGID
}

// Kill is called by the tester when a ShardKV instance won't
// be needed again. you are not required to do anything
// in Kill(), but it might be convenient to (for example)
// turn off debug output from this instance.
func (kv *ShardKV) Kill() {
	kv.rf.Kill()
	// Your code here, if desired.
	atomic.StoreInt32(&kv.dead, 1)
	kv.cfgPollingTicker.Stop()
}

func (kv *ShardKV) killed() bool {
	z := atomic.LoadInt32(&kv.dead)
	return z == 1
}

// StartServer
//
// servers[] contains the ports of the servers in this group.
//
// me is the index of the current server in servers[].
//
// the k/v server should store snapshots through the underlying Raft
// implementation, which should call persister.SaveStateAndSnapshot() to
// atomically save the Raft state along with the snapshot.
//
// the k/v server should snapshot when Raft's saved state exceeds
// maxraftstate bytes, in order to allow Raft to garbage-collect its
// log. if maxraftstate is -1, you don't need to snapshot.
//
// gid is this group's GID, for interacting with the shardctrler.
//
// pass ctrlers[] to shardctrler.MakeClerk() so you can send
// RPCs to the shardctrler.
//
// make_end(servername) turns a server name from a
// Config.Groups[gid][i] into a labrpc.ClientEnd on which you can
// send RPCs. You'll need this to send RPCs to other groups.
//
// look at client.go for examples of how to use ctrlers[]
// and make_end() to send RPCs to the group owning a specific shard.
//
// StartServer() must return quickly, so it should start goroutines
// for any long-running work.
func StartServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister, maxraftstate int, gid int,
	ctrlers []*labrpc.ClientEnd, make_end func(string) *labrpc.ClientEnd) *ShardKV {
	// call labgob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	labgob.Register(Op{})
	labgob.Register(shardctrler.Config{})
	labgob.Register(Shard{})
	labgob.Register(Err(""))

	kv := new(ShardKV)
	//! Initializations
	kv.mu = sync.Mutex{}
	kv.me = me
	kv.maxraftstate = maxraftstate
	kv.make_end = make_end
	kv.gid = gid
	kv.ctrlers = ctrlers
	kv.applyCh = make(chan raft.ApplyMsg)
	kv.rf = raft.Make(servers, me, persister, kv.applyCh)
	// Your initialization code here.
	kv.persister = persister
	kv.dead = 0
	kv.scc = shardctrler.MakeClerk(kv.ctrlers) // TODO: the shardctrler is used for what here?
	kv.lastApplied = 0
	kv.clientId2SerialNum = make(map[int64]uint64)
	kv.opDoneChans = make(map[int]chan Op)
	kv.currCfg = shardctrler.Config{}
	kv.prevCfg = shardctrler.Config{}
	kv.cfgPollingTimeoutMS = 100
	kv.cfgPollingTicker = time.NewTicker(time.Duration(kv.cfgPollingTimeoutMS) * time.Millisecond)
	//kv.reconfigChan = make(chan reconfigMsg, 16)

	kv.clientId = nrand()
	//! Actions
	// init the DB
	for i := 0; i < shardctrler.NShards; i++ {
		kv.inMemoryDB[i] = MakeSubDB(0)
	}
	// read snapshot from persister (after the machine restart)
	snapshot := persister.ReadSnapshot()
	if snapshot != nil && len(snapshot) > 0 {
		clt2SerialNum, db, currCfg, prevCfg, clientId, lastApplied := kv.decodeSnapshot(snapshot)
		kv.clientId2SerialNum = clt2SerialNum
		kv.inMemoryDB = db
		kv.currCfg = currCfg
		kv.prevCfg = prevCfg
		kv.clientId = clientId
		kv.lastApplied = lastApplied
		DebugLog(dSnap, kv, "snapshot installed when server up, currCfg.Num=%v db=%v", currCfg.Num, db2str(kv.inMemoryDB))
	}
	go kv.ticker()
	go kv.applier()
	//go kv.reconfigHandler()
	// debug only
	//go kv.dbgDetectMuDeadLock()
	//go kv.dbgDetecCfgMutexDeadLock()
	DebugLog(dCheck, kv, "server started!")
	return kv
}

func (kv *ShardKV) dbgDetectMuDeadLock() {
	for {
		kv.mu.Lock()
		DebugLog(dCheck, kv, "kv.mu healthy")
		kv.mu.Unlock()
		time.Sleep(500 * time.Millisecond)
	}
}
