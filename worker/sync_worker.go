package worker

import (
	"bytes"
	"context"
	"sync"
	"time"

	"github.com/js361014/roadrunner/v2/payload"
	"github.com/spiral/errors"
	"github.com/spiral/goridge/v3/pkg/frame"
	"github.com/spiral/goridge/v3/pkg/relay"
	"go.uber.org/multierr"
)

// Allocator is responsible for worker allocation in the pool
type Allocator func() (SyncWorker, error)

type SyncWorkerImpl struct {
	process *Process
	fPool   sync.Pool
	bPool   sync.Pool
	chPool  sync.Pool
}

// From creates SyncWorker from BaseProcess
func From(process *Process) *SyncWorkerImpl {
	return &SyncWorkerImpl{
		process: process,
		fPool: sync.Pool{New: func() interface{} {
			return frame.NewFrame()
		}},
		bPool: sync.Pool{New: func() interface{} {
			return new(bytes.Buffer)
		}},

		chPool: sync.Pool{New: func() interface{} {
			return make(chan wexec, 1)
		}},
	}
}

// Exec payload without TTL timeout.
func (tw *SyncWorkerImpl) Exec(p *payload.Payload) (*payload.Payload, error) {
	const op = errors.Op("sync_worker_exec")

	if len(p.Body) == 0 && len(p.Context) == 0 {
		return nil, errors.E(op, errors.Str("payload can not be empty"))
	}

	if tw.process.State().Value() != StateReady {
		return nil, errors.E(op, errors.Errorf("Process is not ready (%s)", tw.process.State().String()))
	}

	// set last used time
	tw.process.State().SetLastUsed(uint64(time.Now().UnixNano()))
	tw.process.State().Set(StateWorking)

	rsp, err := tw.execPayload(p)
	if err != nil {
		// just to be more verbose
		if !errors.Is(errors.SoftJob, err) {
			tw.process.State().Set(StateErrored)
			tw.process.State().RegisterExec()
		}
		return nil, errors.E(op, err)
	}

	// supervisor may set state of the worker during the work
	// in this case we should not re-write the worker state
	if tw.process.State().Value() != StateWorking {
		tw.process.State().RegisterExec()
		return rsp, nil
	}

	tw.process.State().Set(StateReady)
	tw.process.State().RegisterExec()

	return rsp, nil
}

type wexec struct {
	payload *payload.Payload
	err     error
}

// ExecWithTTL executes payload without TTL timeout.
func (tw *SyncWorkerImpl) ExecWithTTL(ctx context.Context, p *payload.Payload) (*payload.Payload, error) {
	const op = errors.Op("sync_worker_exec_worker_with_timeout")

	if len(p.Body) == 0 && len(p.Context) == 0 {
		return nil, errors.E(op, errors.Str("payload can not be empty"))
	}

	c := tw.getCh()
	defer tw.putCh(c)

	// worker was killed before it started to work (supervisor)
	if tw.process.State().Value() != StateReady {
		return nil, errors.E(op, errors.Errorf("Process is not ready (%s)", tw.process.State().String()))
	}
	// set last used time
	tw.process.State().SetLastUsed(uint64(time.Now().UnixNano()))
	tw.process.State().Set(StateWorking)

	go func() {
		rsp, err := tw.execPayload(p)
		if err != nil {
			// just to be more verbose
			if errors.Is(errors.SoftJob, err) == false { //nolint:gosimple
				tw.process.State().Set(StateErrored)
				tw.process.State().RegisterExec()
			}
			c <- wexec{
				err: errors.E(op, err),
			}
			return
		}

		if tw.process.State().Value() != StateWorking {
			tw.process.State().RegisterExec()
			c <- wexec{
				payload: rsp,
				err:     nil,
			}
			return
		}

		tw.process.State().Set(StateReady)
		tw.process.State().RegisterExec()

		c <- wexec{
			payload: rsp,
			err:     nil,
		}
	}()

	select {
	// exec TTL reached
	case <-ctx.Done():
		err := multierr.Combine(tw.Kill())
		if err != nil {
			// append timeout error
			err = multierr.Append(err, errors.E(op, errors.ExecTTL))
			return nil, multierr.Append(err, ctx.Err())
		}
		return nil, errors.E(op, errors.ExecTTL, ctx.Err())
	case res := <-c:
		if res.err != nil {
			return nil, res.err
		}
		return res.payload, nil
	}
}

func (tw *SyncWorkerImpl) execPayload(p *payload.Payload) (*payload.Payload, error) {
	const op = errors.Op("sync_worker_exec_payload")

	// get a frame
	fr := tw.getFrame()
	defer tw.putFrame(fr)

	// can be 0 here
	fr.WriteVersion(fr.Header(), frame.VERSION_1)
	fr.WriteFlags(fr.Header(), p.Codec)

	// obtain a buffer
	buf := tw.get()

	buf.Write(p.Context)
	buf.Write(p.Body)

	// Context offset
	fr.WriteOptions(fr.HeaderPtr(), uint32(len(p.Context)))
	fr.WritePayloadLen(fr.Header(), uint32(buf.Len()))
	fr.WritePayload(buf.Bytes())

	fr.WriteCRC(fr.Header())

	// return buffer
	tw.put(buf)

	err := tw.Relay().Send(fr)
	if err != nil {
		return nil, errors.E(op, errors.Network, err)
	}

	frameR := tw.getFrame()
	defer tw.putFrame(frameR)

	err = tw.process.Relay().Receive(frameR)
	if err != nil {
		return nil, errors.E(op, errors.Network, err)
	}
	if frameR == nil {
		return nil, errors.E(op, errors.Network, errors.Str("nil frame received"))
	}

	flags := frameR.ReadFlags()

	if flags&frame.ERROR != byte(0) {
		return nil, errors.E(op, errors.SoftJob, errors.Str(string(frameR.Payload())))
	}

	options := frameR.ReadOptions(frameR.Header())
	if len(options) != 1 {
		return nil, errors.E(op, errors.Decode, errors.Str("options length should be equal 1 (body offset)"))
	}

	pld := &payload.Payload{
		Codec:   flags,
		Body:    make([]byte, len(frameR.Payload()[options[0]:])),
		Context: make([]byte, len(frameR.Payload()[:options[0]])),
	}

	// by copying we free frame's payload slice
	// we do not hold the pointer from the smaller slice to the initial (which should be in the sync.Pool)
	// https://blog.golang.org/slices-intro#TOC_6.
	copy(pld.Body, frameR.Payload()[options[0]:])
	copy(pld.Context, frameR.Payload()[:options[0]])

	return pld, nil
}

func (tw *SyncWorkerImpl) String() string {
	return tw.process.String()
}

func (tw *SyncWorkerImpl) Pid() int64 {
	return tw.process.Pid()
}

func (tw *SyncWorkerImpl) Created() time.Time {
	return tw.process.Created()
}

func (tw *SyncWorkerImpl) State() State {
	return tw.process.State()
}

func (tw *SyncWorkerImpl) Start() error {
	return tw.process.Start()
}

func (tw *SyncWorkerImpl) Wait() error {
	return tw.process.Wait()
}

func (tw *SyncWorkerImpl) Stop() error {
	return tw.process.Stop()
}

func (tw *SyncWorkerImpl) Kill() error {
	return tw.process.Kill()
}

func (tw *SyncWorkerImpl) Relay() relay.Relay {
	return tw.process.Relay()
}

func (tw *SyncWorkerImpl) AttachRelay(rl relay.Relay) {
	tw.process.AttachRelay(rl)
}

// Private

func (tw *SyncWorkerImpl) get() *bytes.Buffer {
	return tw.bPool.Get().(*bytes.Buffer)
}

func (tw *SyncWorkerImpl) put(b *bytes.Buffer) {
	b.Reset()
	tw.bPool.Put(b)
}

func (tw *SyncWorkerImpl) getFrame() *frame.Frame {
	return tw.fPool.Get().(*frame.Frame)
}

func (tw *SyncWorkerImpl) putFrame(f *frame.Frame) {
	f.Reset()
	tw.fPool.Put(f)
}

func (tw *SyncWorkerImpl) getCh() chan wexec {
	return tw.chPool.Get().(chan wexec)
}

func (tw *SyncWorkerImpl) putCh(ch chan wexec) {
	// just check if the chan is not empty
	select {
	case <-ch:
		tw.chPool.Put(ch)
	default:
		tw.chPool.Put(ch)
	}
}
