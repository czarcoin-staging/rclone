// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcmanager

import (
	"context"
	"fmt"
	"sync"

	"github.com/zeebo/errs"

	"storj.io/drpc"
	"storj.io/drpc/drpcctx"
	"storj.io/drpc/drpcdebug"
	"storj.io/drpc/drpcsignal"
	"storj.io/drpc/drpcstream"
	"storj.io/drpc/drpcwire"
)

var (
	managerClosed      = errs.New("manager closed")
	manageStreamExited = errs.New("manage stream exited")
)

type Manager struct {
	tr drpc.Transport
	wr *drpcwire.Writer
	rd *drpcwire.Reader

	once sync.Once

	sid   uint64
	sem   chan struct{}
	term  drpcsignal.Signal // set when the manager should start terminating
	read  drpcsignal.Signal // set after the goroutine reading from the transport is done
	tport drpcsignal.Signal // set after the transport has been closed
	queue chan drpcwire.Packet
	ctx   context.Context
}

func New(tr drpc.Transport) *Manager {
	m := &Manager{
		tr: tr,
		wr: drpcwire.NewWriter(tr, 1024),
		rd: drpcwire.NewReader(tr),

		// this semaphore controls the number of concurrent streams. it MUST be 1.
		sem:   make(chan struct{}, 1),
		queue: make(chan drpcwire.Packet),
		ctx:   drpcctx.WithTransport(context.Background(), tr),
	}

	go m.manageTransport()
	go m.manageReader()

	return m
}

//
// helpers
//

func poll(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (m *Manager) poll(ctx context.Context) error {
	switch {
	case poll(ctx.Done()):
		return ctx.Err()

	case poll(m.term.Signal()):
		return m.term.Err()

	default:
		return nil
	}
}

func (m *Manager) acquireSemaphore(ctx context.Context) error {
	if err := m.poll(ctx); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()

	case <-m.term.Signal():
		return m.term.Err()

	case m.sem <- struct{}{}:
		return nil
	}
}

//
// exported interface
//

func (m *Manager) Closed() bool {
	return m.term.IsSet()
}

func (m *Manager) Close() error {
	// when closing, we set the manager terminated signal, wait for the goroutine
	// managing the transport to notice and close it, acquire the semaphore to ensure
	// there are streams running, then wait for the goroutine reading packets to be done.
	// we protect it with a once to ensure both that we only do this once, and that
	// concurrent calls are sure that it has fully executed.

	m.once.Do(func() {
		m.term.Set(managerClosed)
		<-m.tport.Signal()
		m.sem <- struct{}{}
		<-m.read.Signal()
	})

	return m.tport.Err()
}

func (m *Manager) NewClientStream(ctx context.Context) (stream *drpcstream.Stream, err error) {
	if err := m.acquireSemaphore(ctx); err != nil {
		return nil, err
	}

	m.sid++
	stream = drpcstream.New(m.ctx, m.sid, m.wr)
	go m.manageStream(ctx, stream)
	return stream, nil
}

func (m *Manager) NewServerStream(ctx context.Context) (stream *drpcstream.Stream, rpc string, err error) {
	if err := m.acquireSemaphore(ctx); err != nil {
		return nil, "", err
	}

	for {
		select {
		case <-ctx.Done():
			<-m.sem
			return nil, "", ctx.Err()

		case <-m.term.Signal():
			<-m.sem
			return nil, "", m.term.Err()

		case pkt := <-m.queue:
			// we ignore packets that arent invokes because perhaps older streams have
			// messages in the queue sent concurrently with our notification to them
			// that the stream they were sent for is done.
			if pkt.Kind != drpcwire.Kind_Invoke {
				continue
			}

			stream = drpcstream.New(m.ctx, pkt.ID.Stream, m.wr)
			go m.manageStream(ctx, stream)
			return stream, string(pkt.Data), nil
		}
	}
}

//
// manage transport
//

// manageTransport ensures that if the manager's done signal is ever set, then
// the underlying transport is closed and the error is recorded.
func (m *Manager) manageTransport() {
	<-m.term.Signal()
	m.tport.Set(m.tr.Close())
}

//
// manage reader
//

// manageReader is always reading a packet and sending it into the queue of packets
// the manager has. It sets the rdSig signal when it exits so that one can wait to
// ensure that no one is reading on the reader. It sets the done signal if there is
// any error reading packets.
func (m *Manager) manageReader() {
	defer m.read.Set(managerClosed)

	for {
		pkt, err := m.rd.ReadPacket()
		if err != nil {
			m.term.Set(errs.Wrap(err))
			return
		}

		drpcdebug.Log(func() string { return fmt.Sprintf("MAN[%p]: %v", m, pkt) })

		select {
		case <-m.term.Signal():
			return

		case m.queue <- pkt:
		}
	}
}

//
// manage stream
//

func (m *Manager) manageStream(ctx context.Context, stream *drpcstream.Stream) {
	// create a wait group, launch the workers, and wait for them
	wg := new(sync.WaitGroup)
	wg.Add(2)
	go m.manageStreamPackets(wg, ctx, stream)
	go m.manageStreamContext(wg, ctx, stream)
	wg.Wait()

	// always ensure the stream is terminated if we're done managing it. the
	// stream should already be in a terminal state unless we're exiting due
	// to the manager terminating. that only happens if the underlying transport
	// died, so just assume the remote end issued a cancel by terminating
	// the transport.
	stream.Cancel(context.Canceled)

	// release semaphore
	<-m.sem
}

// manageStreamPackets repeatedly reads from the queue of packets and asks the stream to
// handle them. If there is an error handling a packet, that is considered to
// be fatal to the manager, so we set done. HandlePacket also returns a bool to
// indicate that the stream requires no more packets, and so manageStream can
// just exit. It releases the semaphore whenever it exits.
func (m *Manager) manageStreamPackets(wg *sync.WaitGroup, ctx context.Context, stream *drpcstream.Stream) {
	defer wg.Done()

	for {
		select {
		case <-m.term.Signal():
			return

		case <-stream.Terminated():
			return

		case pkt := <-m.queue:
			drpcdebug.Log(func() string { return fmt.Sprintf("FWD[%p][%p]: %v", m, stream, pkt) })

			err, ok := stream.HandlePacket(pkt)
			if err != nil {
				m.term.Set(errs.Wrap(err))
				return
			} else if !ok {
				return
			}
		}
	}
}

// manageStreamContext ensures that if the stream context is canceled, we inform the stream and
// possibly abort the underlying transport if the stream isn't finished.
func (m *Manager) manageStreamContext(wg *sync.WaitGroup, ctx context.Context, stream *drpcstream.Stream) {
	defer wg.Done()

	select {
	case <-m.term.Signal():
		return

	case <-stream.Terminated():
		return

	case <-ctx.Done():
		stream.Cancel(ctx.Err())
		if !stream.Finished() {
			m.term.Set(ctx.Err())
		}
	}
}
