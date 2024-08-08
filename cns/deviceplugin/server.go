package deviceplugin

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/pkg/errors"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

type deviceCounter interface {
	getDeviceCount() int
}

type Server struct {
	address              string
	logger               *zap.Logger
	deviceCounter        deviceCounter
	listAndWatchDoneChan chan struct{}
	deviceCheckInterval  time.Duration
}

func NewServer(address string, logger *zap.Logger, deviceCounter deviceCounter, deviceCheckInterval time.Duration) *Server {
	return &Server{
		address:              address,
		logger:               logger,
		deviceCounter:        deviceCounter,
		listAndWatchDoneChan: make(chan struct{}),
		deviceCheckInterval:  deviceCheckInterval,
	}
}

// Run starts the grpc server and blocks until an error or context is cancelled. Wait on Ready to know when the server is ready.
func (s *Server) Run(ctx context.Context) error {
	grpcServer := grpc.NewServer()
	v1beta1.RegisterDevicePluginServer(grpcServer, s)
	defer close(s.listAndWatchDoneChan)

	l, err := net.Listen("unix", s.address)
	if err != nil {
		return errors.Wrap(err, "error listening on socket")
	}
	errChan := make(chan error, 2) //nolint:gomnd // disabled in favor of readability
	go func() {
		errChan <- grpcServer.Serve(l)
	}()
	defer grpcServer.Stop()

	select {
	case err := <-errChan:
		return errors.Wrap(err, "error running grpc server")
	case <-ctx.Done():
		return nil
	}
}

// Ready blocks until the server is ready
func (s *Server) Ready(ctx context.Context) error {
	c, err := grpc.DialContext(ctx, s.address, grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock(), //nolint:staticcheck // TODO: Move to grpc.NewClient method
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			conn, err := (&net.Dialer{}).DialContext(ctx, "unix", addr)
			if err != nil {
				return nil, errors.Wrap(err, "failed to dial context")
			}
			return conn, nil
		}),
	)
	if err != nil {
		return errors.Wrap(err, "error dialing local grpc server")
	}
	if err := c.Close(); err != nil {
		return errors.Wrap(err, "error closing connection to local grpc server")
	}
	return nil
}

func (s *Server) Allocate(req *v1beta1.AllocateRequest) (*v1beta1.AllocateResponse, error) {
	s.logger.Info("allocate request", zap.Any("req", *req))
	resps := make([]*v1beta1.ContainerAllocateResponse, len(req.ContainerRequests))
	for i, containerReq := range req.ContainerRequests {
		resp := &v1beta1.ContainerAllocateResponse{
			Envs: make(map[string]string),
		}
		for j := range containerReq.DevicesIDs {
			resp.Envs[fmt.Sprintf("NIC_%d", j)] = containerReq.DevicesIDs[j]
		}
		resps[i] = resp
	}
	r := &v1beta1.AllocateResponse{
		ContainerResponses: resps,
	}
	return r, nil
}

func (s *Server) ListAndWatch(_ *v1beta1.Empty, stream v1beta1.DevicePlugin_ListAndWatchServer) error {
	// send the initial count right away
	advertisedCount := s.deviceCounter.getDeviceCount()
	devices := make([]*v1beta1.Device, advertisedCount)
	for i := range devices {
		devices[i] = &v1beta1.Device{
			ID:     fmt.Sprintf("NIC-%d", i),
			Health: v1beta1.Healthy,
		}
	}
	if err := stream.Send(&v1beta1.ListAndWatchResponse{
		Devices: devices,
	}); err != nil {
		return errors.Wrap(err, "error sending listAndWatch response")
	}

	// every interval, check if the current count has changed from what we've previously sent, and if so, send the new count
	for {
		select {
		case <-s.listAndWatchDoneChan:
			return nil
		case <-time.After(s.deviceCheckInterval):
			currentCount := s.deviceCounter.getDeviceCount()
			if currentCount == advertisedCount {
				continue
			}
			advertisedCount = currentCount
			devices := make([]*v1beta1.Device, advertisedCount)
			for i := range devices {
				devices[i] = &v1beta1.Device{
					ID:     fmt.Sprintf("vnet-nic-%d", i),
					Health: v1beta1.Healthy,
				}
			}
			if err := stream.Send(&v1beta1.ListAndWatchResponse{
				Devices: devices,
			}); err != nil {
				return errors.Wrap(err, "error sending listAndWatch response")
			}
		}
	}
}

func (s *Server) GetDevicePluginOptions(context.Context, *v1beta1.Empty) (*v1beta1.DevicePluginOptions, error) {
	return &v1beta1.DevicePluginOptions{}, nil
}

func (s *Server) GetPreferredAllocation(context.Context, *v1beta1.PreferredAllocationRequest) (*v1beta1.PreferredAllocationResponse, error) {
	return &v1beta1.PreferredAllocationResponse{}, nil
}

func (s *Server) PreStartContainer(context.Context, *v1beta1.PreStartContainerRequest) (*v1beta1.PreStartContainerResponse, error) {
	return &v1beta1.PreStartContainerResponse{}, nil
}
