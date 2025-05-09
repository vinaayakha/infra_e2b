//go:build linux
// +build linux

package nbd

import (
	"context"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/Merovius/nbd/nbdnl"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"

	"github.com/e2b-dev/infra/packages/orchestrator/internal/sandbox/block"
	"github.com/e2b-dev/infra/packages/shared/pkg/telemetry"
)

type DirectPathMount struct {
	tracer      trace.Tracer
	Backend     block.Device
	ctx         context.Context
	dispatcher  *Dispatch
	conn        net.Conn
	deviceIndex uint32
	blockSize   uint64
	cancelfn    context.CancelFunc
	devicePool  *DevicePool
}

func NewDirectPathMount(tracer trace.Tracer, b block.Device, devicePool *DevicePool) *DirectPathMount {
	ctx, cancelfn := context.WithCancel(context.Background())

	return &DirectPathMount{
		tracer:     tracer,
		Backend:    b,
		ctx:        ctx,
		cancelfn:   cancelfn,
		blockSize:  4096,
		devicePool: devicePool,
	}
}

func (d *DirectPathMount) Open(ctx context.Context) (uint32, error) {
	size, err := d.Backend.Size()
	if err != nil {
		return 0, err
	}

	for {
		d.deviceIndex, err = d.devicePool.GetDevice(ctx)
		if err != nil {
			return 0, err
		}

		// Create the socket pairs
		sockPair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
		if err != nil {
			return 0, err
		}

		client := os.NewFile(uintptr(sockPair[0]), "client")
		server := os.NewFile(uintptr(sockPair[1]), "server")
		d.conn, err = net.FileConn(server)

		if err != nil {
			return 0, err
		}
		server.Close()

		d.dispatcher = NewDispatch(d.ctx, d.conn, d.Backend)
		// Start reading commands on the socket and dispatching them to our provider
		go func() {
			handleErr := d.dispatcher.Handle()
			if handleErr != nil {
				zap.L().Error("error handling NBD commands", zap.Error(handleErr))
			}
		}()

		var opts []nbdnl.ConnectOption
		opts = append(opts, nbdnl.WithBlockSize(d.blockSize))
		opts = append(opts, nbdnl.WithTimeout(5*time.Second))
		opts = append(opts, nbdnl.WithDeadconnTimeout(5*time.Second))

		serverFlags := nbdnl.FlagHasFlags | nbdnl.FlagCanMulticonn

		idx, err := nbdnl.Connect(d.deviceIndex, []*os.File{client}, uint64(size), 0, serverFlags, opts...)
		if err == nil {
			d.deviceIndex = idx
			break
		}

		// Sometimes (rare), there seems to be a BADF error here. Lets just retry for now...
		// Close things down and try again...
		_ = client.Close()

		connErr := d.conn.Close()
		if connErr != nil {
			zap.L().Error("error closing conn", zap.Error(connErr))
		}

		releaseErr := d.devicePool.ReleaseDevice(d.deviceIndex)
		if releaseErr != nil {
			zap.L().Error("error releasing device", zap.Error(releaseErr))
		}

		d.deviceIndex = 0

		if strings.Contains(err.Error(), "invalid argument") {
			return 0, err
		}

		time.Sleep(25 * time.Millisecond)
	}

	// Wait until it's connected...
	for {
		select {
		case <-d.ctx.Done():
			return 0, d.ctx.Err()
		default:
		}

		s, err := nbdnl.Status(d.deviceIndex)
		if err == nil && s.Connected {
			break
		}

		time.Sleep(100 * time.Nanosecond)
	}

	return d.deviceIndex, nil
}

func (d *DirectPathMount) Close(ctx context.Context) error {
	childCtx, childSpan := d.tracer.Start(ctx, "direct-path-mount-close")
	defer childSpan.End()

	// First cancel the context, which will stop waiting on pending readAt/writeAt...
	telemetry.ReportEvent(childCtx, "canceling context")
	d.cancelfn()

	// Now wait for any pending responses to be sent
	telemetry.ReportEvent(childCtx, "waiting for pending responses")
	if d.dispatcher != nil {
		d.dispatcher.Wait()
	}

	// Now ask to disconnect
	telemetry.ReportEvent(childCtx, "disconnecting NBD")
	err := nbdnl.Disconnect(d.deviceIndex)
	if err != nil {
		return err
	}

	// Close all the socket pairs...
	telemetry.ReportEvent(childCtx, "closing socket pairs")
	err = d.conn.Close()
	if err != nil {
		return err
	}

	// Wait until it's completely disconnected...
	telemetry.ReportEvent(childCtx, "waiting for complete disconnection")
	ctxTimeout, cancel := context.WithTimeout(childCtx, 4*time.Second)
	defer cancel()
	for {
		select {
		case <-ctxTimeout.Done():
			return ctxTimeout.Err()
		default:
		}

		s, err := nbdnl.Status(d.deviceIndex)
		if err == nil && !s.Connected {
			break
		}

		time.Sleep(100 * time.Nanosecond)
	}

	return nil
}
