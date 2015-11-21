package store

import (
	"fmt"
	"time"
)

type GCMgr struct {
	bucketID int
	stat     *GCState // curr or laste
	ki       KeyInfo
}

type GCState struct {
	BeginTS time.Time
	EndTS   time.Time

	// Begin and End are chunckIDs, they determine the range of GC.
	Begin int
	End   int

	// Src and Dst are chunkIDs, they are tmp variables used in gc process.
	Src int
	Dst int

	Err     error
	Running bool

	// sum
	GCFileState
}

type GCFileState struct {
	Src                int
	NumBefore          int
	NumReleased        int
	NumReleasedDeleted int
	SizeBefore         uint32
	SizeReleased       uint32
	SizeDeleted        uint32
	SizeBroken         uint32
}

func (s *GCFileState) add(size uint32, isRetained, isDeleted bool, sizeBroken uint32) {
	if !isRetained {
		s.NumReleased += 1
		s.SizeReleased += size
		if isDeleted {
			s.NumReleasedDeleted += 1
		}
	}
	s.SizeReleased += sizeBroken
	s.SizeBroken += sizeBroken
	s.SizeBefore += (size + sizeBroken)
	s.NumBefore += 1
}

func (s *GCFileState) String() string {
	return fmt.Sprintf("%#v", s)
}

func (mgr *GCMgr) ShouldRetainRecord(bkt *Bucket, rec *Record, oldPos Position) (retain, isCollision, isDeleted bool) {
	ki := &mgr.ki
	ki.KeyHash = getKeyHash(rec.Key)
	ki.Key = rec.Key
	ki.StringKey = string(ki.Key)
	ki.KeyIsPath = false
	ki.Prepare()
	meta, pos, found := bkt.htree.get(ki)
	if !found {
		logger.Errorf("gc old key not found in htree bucket %d %#v %#v %#v",
			bkt.id, ki, meta, oldPos)
		return true, false, false
	} else if pos == oldPos {
		return true, false, meta.Ver < 0
	} else {
		it, collision := bkt.hints.collisions.get(ki.KeyHash, ki.StringKey)
		if !collision {
			// only in mem, in new hints buffers after gc begin
			it, collision = bkt.hints.getItemCollision(ki.KeyHash, ki.StringKey)
		}
		if collision {
			if it != nil {
				return decodePos(it.Pos) == oldPos, true, it.Ver < 0
			} else {
				return true, true, false
			}
		}
	}
	return false, false, false
}

func (mgr *GCMgr) UpdateCollision(bkt *Bucket, ki *KeyInfo, oldPos, newPos Position, rec *Record) {
	// not have to (leave it to get)

	// if in ctable: update pos
	// else: decompress, get vhash and set collisions
}

func (mgr *GCMgr) UpdateHtreePos(bkt *Bucket, ki *KeyInfo, oldPos, newPos Position) {
	// TODO: should be a api of htree to be atomic
	meta, pos, _ := bkt.htree.get(ki)
	if pos != oldPos {
		logger.Warnf("old key update when updating pos bucket %d %s %#v %#v",
			bkt.id, ki.StringKey, meta, oldPos)
		return
	}
	bkt.htree.set(ki, meta, newPos)
}

func (mgr *GCMgr) BeforeBucket(bkt *Bucket, startChunkID, endChunkID int) {
	bkt.hints.state |= HintStateGC // will about
	for bkt.hints.state&HintStateMerge != 0 {
		logger.Infof("gc wait for merge to stop")
		time.Sleep(5 * time.Millisecond)
	}

	// dump hint and do merge, and hold all new SETs in hint buffers
	// so collision will be find either during merge or in hint buffer
	// so will not wrongly GC a collision record. e.g.:
	//   key1 and key2 have the same keyhash, key1 is set before gc, and key2 after that.
	bkt.hints.maxDumpableChunkID = endChunkID - 1
	bkt.hints.forceRotateSplit()
	time.Sleep(time.Duration(SecsBeforeDump+1) * time.Second)
	bkt.hints.dumpAndMerge(true) // TODO: should not dump idx.m!
	bkt.hints.Merge(true)

	// remove hints
	bkt.removeHtree()
}

func (mgr *GCMgr) AfterBucket(bkt *Bucket) {
	bkt.hints.state &= ^HintStateGC
	bkt.hints.maxDumpableChunkID = MAX_CHUNK_ID
	bkt.dumpHtree()
}

func (mgr *GCMgr) gc(bkt *Bucket, startChunkID, endChunkID int) (err error) {
	if endChunkID < 0 || endChunkID >= bkt.datas.newHead {
		endChunkID = bkt.datas.newHead - 1
	}
	logger.Infof("begin GC bucket %d chunk [%d, %d]", bkt.id, startChunkID, endChunkID)

	bkt.GCHistory = append(bkt.GCHistory, GCState{})
	gc := &bkt.GCHistory[len(bkt.GCHistory)-1]
	mgr.stat = gc
	gc.Running = true
	defer func() {
		gc.Running = false
	}()
	gc.Begin = startChunkID
	gc.End = endChunkID

	var oldPos Position
	var newPos Position
	var rec *Record
	var r *DataStreamReader
	var w *DataStreamWriter
	mfs := uint32(conf.DataFileMax)

	mgr.BeforeBucket(bkt, startChunkID, endChunkID)
	defer mgr.AfterBucket(bkt)

	gc.Dst = startChunkID
	for gc.Src = gc.Begin; gc.Src <= gc.End; gc.Src++ {
		oldPos.ChunkID = gc.Src
		var fileState GCFileState
		// reader must have a larger buffer
		logger.Infof("begin GC bucket %d, file %d -> %d", bkt.id, gc.Src, gc.Dst)
		bkt.hints.ClearChunks(gc.Src, gc.Src)
		if r, err = bkt.datas.GetStreamReader(gc.Src); err != nil {
			gc.Err = err
			logger.Errorf("gc failed: %s", err.Error())
			return
		}
		w, err = bkt.datas.GetStreamWriter(gc.Dst, gc.Dst != gc.Src)
		if err != nil {
			gc.Err = err
			return
		}
		for {
			var sizeBroken uint32
			rec, oldPos.Offset, sizeBroken, err = r.Next()
			if err != nil {
				gc.Err = err
				logger.Errorf("gc failed: %s", err.Error())
				return
			}
			if rec == nil {
				break
			}

			_, recsize := rec.Sizes()

			if recsize+w.Offset() > mfs {
				w.Close()
				gc.Dst++
				newPos.ChunkID = gc.Dst
				if w, err = bkt.datas.GetStreamWriter(gc.Dst, gc.Dst != gc.Src); err != nil {
					gc.Err = err
					logger.Errorf("gc failed: %s", err.Error())
					return
				}
			}
			isRetained, isCollision, isDeleted := mgr.ShouldRetainRecord(bkt, rec, oldPos)
			if isRetained {
				//	logger.Infof("retain %s %s", string(rec.Key), string(rec.Payload.Body))
				if newPos.Offset, err = w.Append(rec); err != nil {
					gc.Err = err
					logger.Errorf("gc failed: %s", err.Error())
					return
				}
				keyinfo := NewKeyInfoFromBytes(rec.Key, getKeyHash(rec.Key), false)
				if isCollision {
					mgr.UpdateCollision(bkt, keyinfo, oldPos, newPos, rec)
				} else {
					mgr.UpdateHtreePos(bkt, keyinfo, oldPos, newPos)
				}
			} else {
				//	logger.Infof("drop %s %s", string(rec.Key), string(rec.Payload.Body))
			}
			fileState.add(recsize, isRetained, isDeleted, sizeBroken)
		}
		w.Close()
		size := w.Offset()
		bkt.datas.Truncate(gc.Dst, size)
		if gc.Src != gc.Dst {
			bkt.datas.DeleteFile(gc.Src)
		}
		logger.Infof("end GC file %#v", fileState)
	}
	return nil
}
