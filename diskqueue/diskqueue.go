package diskqueue

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"sync/atomic"

	"github.com/zhiqiangxu/util"
	"github.com/zhiqiangxu/util/logger"
	"github.com/zhiqiangxu/util/mapped"
	"go.uber.org/zap"
)

type queueInterface interface {
	Init() error
	Put([]byte) (int64, error)
	Read(offset int64, stores [][]byte) (results [][]byte, err error)
	Close() error
	Delete() error
}

var _ queueInterface = (*Queue)(nil)

// Queue for diskqueue
type Queue struct {
	closeState uint32
	wg         sync.WaitGroup
	meta       *queueMeta
	conf       Conf
	writeCh    chan *writeRequest
	writeReqs  []*writeRequest
	writeBuffs net.Buffers
	sizeBuffs  []byte
	doneCh     chan struct{}
	files      []*qfile
}

const (
	defaultWriteBatch = 1
	defaultMaxMsgSize = 512 * 1024 * 1024
)

// New is ctor for Queue
func New(conf Conf) *Queue {
	if conf.WriteBatch <= 0 {
		conf.WriteBatch = defaultWriteBatch
	}
	if conf.MaxMsgSize <= 0 {
		conf.MaxMsgSize = defaultMaxMsgSize
	}

	q := &Queue{conf: conf, writeCh: make(chan *writeRequest, conf.WriteBatch), writeReqs: make([]*writeRequest, 0, conf.WriteBatch), writeBuffs: make(net.Buffers, 0, conf.WriteBatch*2), sizeBuffs: make([]byte, 4*conf.WriteBatch), doneCh: make(chan struct{})}
	q.meta = newQueueMeta(&q.conf)
	return q
}

const (
	dirPerm = 0660
)

// Init the queue
func (q *Queue) Init() (err error) {

	// 确保各种目录存在
	err = os.MkdirAll(filepath.Join(q.conf.Directory, qfSubDir), dirPerm)
	if err != nil {
		return
	}

	// 初始化元数据
	err = q.meta.Init()
	if err != nil {
		return
	}

	// 加载qfile
	nFiles := q.meta.NumFiles()
	q.files = make([]*qfile, 0, nFiles)
	var qf *qfile
	for i := 0; i < nFiles; i++ {
		qf, err = openQfile(q.meta, i)
		if err != nil {
			return
		}
		if i < (nFiles - 1) {
			err = qf.Shrink()
			if err != nil {
				return
			}
		}
		q.files = append(q.files, qf)
	}

	// enough data, ready to go!
	if len(q.files) == 0 {
		err = q.createQfile()
		if err != nil {
			logger.Instance().Error("Init createQfile", zap.Error(err))
			return
		}
	}

	util.GoFunc(&q.wg, q.handleWrite)
	util.GoFunc(&q.wg, q.handleCommit)

	return nil
}

func (q *Queue) createQfile() (err error) {
	var qf *qfile
	if len(q.files) == 0 {
		qf, err = createQfile(q.meta, 0, 0)
		if err != nil {
			return
		}
	} else {
		qf, err = createQfile(q.meta, len(q.files), q.files[len(q.files)-1].WrotePosition())
		if err != nil {
			return
		}
	}
	q.files = append(q.files, qf)
	return
}

type writeResult struct {
	err    error
	offset int64
}
type writeRequest struct {
	data   []byte
	result chan writeResult
}

var wreqPool = sync.Pool{New: func() interface{} {
	return &writeRequest{result: make(chan writeResult, 1)}
}}

// dedicated G so that write is serial
func (q *Queue) handleWrite() {
	var (
		wreq           *writeRequest
		qf             *qfile
		err            error
		wroteN, totalN int64
	)

	startFM := q.meta.FileMeta(len(q.files) - 1)
	startWrotePosition := startFM.EndOffset

	for {
		select {
		case <-q.doneCh:
			return
		case wreq = <-q.writeCh:
			q.writeReqs = q.writeReqs[:0]
			q.writeBuffs = q.writeBuffs[:0]
			q.writeReqs = append(q.writeReqs, wreq)
			q.updateSizeBuf(0, len(wreq.data))
			q.writeBuffs = append(q.writeBuffs, q.getSizeBuf(0))
			q.writeBuffs = append(q.writeBuffs, wreq.data)

			// collect more data
		BatchLoop:
			for i := 0; i < q.conf.WriteBatch-1; i++ {
				select {
				case wreq = <-q.writeCh:
					q.writeReqs = append(q.writeReqs, wreq)
					q.updateSizeBuf(i+1, len(wreq.data))
					q.writeBuffs = append(q.writeBuffs, q.getSizeBuf(i+1))
					q.writeBuffs = append(q.writeBuffs, wreq.data)
				default:
					break BatchLoop
				}
			}

			// enough data, ready to go!
			qf = q.files[len(q.files)-1]

			writeBuffs := q.writeBuffs

			util.TryUntilSuccess(func() bool {
				wroteN, err = q.writeBuffs.WriteTo(qf)
				totalN += wroteN
				if err == mapped.ErrWriteBeyond {
					// 写超了，需要新开文件
					err = q.createQfile()
					if err != nil {
						logger.Instance().Error("handleWrite createQfile", zap.Error(err))
					} else {
						qf = q.files[len(q.files)-1]
						wroteN, err = q.writeBuffs.WriteTo(qf)
						totalN += wroteN
					}
				}
				if err != nil {
					logger.Instance().Error("handleWrite WriteTo", zap.Error(err))
					return false
				}
				return true
			}, time.Second)

			q.meta.UpdateFileStat(len(q.files)-1, len(q.writeReqs), startWrotePosition+totalN, NowNano())
			totalN = 0
			q.writeBuffs = writeBuffs

			// 全部写入成功
			for _, req := range q.writeReqs {
				req.result <- writeResult{}
			}

		}
	}
}

func (q *Queue) getSizeBuf(i int) []byte {
	return q.sizeBuffs[4*i : 4*i+4]
}

func (q *Queue) updateSizeBuf(i int, size int) {
	binary.BigEndian.PutUint32(q.sizeBuffs[4*i:], uint32(size))
}

func (q *Queue) handleCommit() {
	if !q.conf.EnableWriteBuffer {
		return
	}

	ticker := time.NewTicker(time.Second)

	for {
		select {
		case <-ticker.C:
		case <-q.doneCh:
			return
		}
	}
}

// Put data to queue
func (q *Queue) Put(data []byte) (offset int64, err error) {

	err = q.checkCloseState()
	if err != nil {
		return
	}

	if len(data) > q.conf.MaxMsgSize {
		err = errMsgTooLarge
		return
	}

	wreq := wreqPool.Get().(*writeRequest)
	wreq.data = data
	if len(wreq.result) > 0 {
		<-wreq.result
	}

	select {
	case q.writeCh <- wreq:
		result := <-wreq.result
		offset = result.offset
		err = result.err
		return
	case <-q.doneCh:
		err = errAlreadyClosed
		return
	}

}

// ReadFrom for read from offset
func (q *Queue) Read(offset int64, stores [][]byte) (results [][]byte, err error) {
	err = q.checkCloseState()
	if err != nil {
		return
	}

	return
}

var (
	errAlreadyClosed  = errors.New("already closed")
	errAlreadyClosing = errors.New("already closing")
	errMsgTooLarge    = errors.New("msg too large")
)

const (
	open uint32 = iota
	closing
	closed
)

func (q *Queue) checkCloseState() (err error) {
	closeState := atomic.LoadUint32(&q.closeState)
	switch closeState {
	case open:
	case closing:
		err = errAlreadyClosing
	case closed:
		err = errAlreadyClosed
	default:
		err = fmt.Errorf("unknown close state:%d", closeState)
	}
	return
}

// Close the queue
func (q *Queue) Close() (err error) {

	swapped := atomic.CompareAndSwapUint32(&q.closeState, open, closing)
	if !swapped {
		return q.checkCloseState()
	}

	util.TryUntilSuccess(func() bool {
		// try until success
		err = q.meta.Close()
		if err != nil {
			logger.Instance().Error("meta.Close", zap.Error(err))
			return false
		}

		return true
		// need human interfere

	}, time.Second)

	close(q.doneCh)
	q.wg.Wait()
	atomic.StoreUint32(&q.closeState, closed)

	return
}

// Delete the queue
func (q *Queue) Delete() error {
	return nil
}