package deviceplugin_test

import (
	"context"
	"net"
	"os"
	"path"
	"testing"
	"time"

	"github.com/Azure/azure-container-networking/cns/deviceplugin"
	"github.com/Azure/azure-container-networking/crd/multitenancy/api/v1alpha1"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

func TestPluginManagerStartStop(t *testing.T) {
	logger, err := zap.NewDevelopment()
	if err != nil {
		t.Fatalf("error getting logger: %v", err)
	}

	// start up the fake kubelet
	fakeKubeletSocketDir := os.TempDir()
	kubeletSocket := path.Join(fakeKubeletSocketDir, "kubelet.sock")
	kubeletErrChan := make(chan error)
	vnetPluginRegisterChan := make(chan string)
	ibPluginRegisterChan := make(chan string)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		err := runFakeKubelet(ctx, kubeletSocket, vnetPluginRegisterChan, ibPluginRegisterChan, fakeKubeletSocketDir)
		kubeletErrChan <- err
	}()

	// run the plugin manager
	expectedVnetNICs := 2
	expectedIBNICs := 3
	manager := deviceplugin.NewPluginManager(logger, expectedVnetNICs, expectedIBNICs,
		deviceplugin.PluginManagerSocketPrefix(fakeKubeletSocketDir),
		deviceplugin.PluginManagerKubeletSocket(kubeletSocket),
		deviceplugin.PluginDeviceCheckInterval(time.Second))

	errChan := make(chan error)
	go func() {
		errChan <- manager.Run(ctx)
	}()

	// wait till the two plugins register themselves with fake kubelet
	vnetPluginEndpoint := <-vnetPluginRegisterChan
	ibPluginEndpoint := <-ibPluginRegisterChan

	// assert the plugin reports the expected vnet nic count
	gotVnetNICCount := getDeviceCount(t, vnetPluginEndpoint)
	if gotVnetNICCount != expectedVnetNICs {
		t.Fatalf("expected %d vnet nics but got %d", expectedVnetNICs, gotVnetNICCount)
	}
	gotIBNICCount := getDeviceCount(t, ibPluginEndpoint)
	if gotIBNICCount != expectedIBNICs {
		t.Fatalf("expected %d ib nics but got %d", expectedIBNICs, gotIBNICCount)
	}

	// update the device counts and assert they match expected after some time
	expectedVnetNICs = 5
	expectedIBNICs = 6
	manager.TrackDevices(v1alpha1.DeviceTypeVnetNIC, expectedVnetNICs)
	manager.TrackDevices(v1alpha1.DeviceTypeInfiniBandNIC, expectedIBNICs)
	<-time.After(3 * time.Second)
	gotVnetNICCount = getDeviceCount(t, vnetPluginEndpoint)
	if gotVnetNICCount != expectedVnetNICs {
		t.Fatalf("expected %d vnet nics but got %d", expectedVnetNICs, gotVnetNICCount)
	}
	gotIBNICCount = getDeviceCount(t, ibPluginEndpoint)
	if gotIBNICCount != expectedIBNICs {
		t.Fatalf("expected %d ib nics but got %d", expectedIBNICs, gotIBNICCount)
	}

	// shut down the plugin manager and fake kubelet
	cancel()

	// ensure the plugin manager didn't report an error
	if err := <-errChan; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// ensure the fake kubelet didn't report an error
	if err := <-kubeletErrChan; err != nil {
		t.Fatalf("unexpected error from fake kubelet: %v", err)
	}
}

type fakeKubelet struct {
	vnetPluginRegisterChan chan string
	ibPluginRegisterChan   chan string
	pluginPrefix           string
}

func (f *fakeKubelet) Register(_ context.Context, req *v1beta1.RegisterRequest) (*v1beta1.Empty, error) {
	switch req.ResourceName {
	case string(v1alpha1.DeviceTypeVnetNIC):
		f.vnetPluginRegisterChan <- path.Join(f.pluginPrefix, req.Endpoint)
	case string(v1alpha1.DeviceTypeInfiniBandNIC):
		f.ibPluginRegisterChan <- path.Join(f.pluginPrefix, req.Endpoint)
	}
	return &v1beta1.Empty{}, nil
}

func runFakeKubelet(ctx context.Context, address string, vnetPluginRegisterChan, ibPluginRegisterChan chan string, pluginPrefix string) error {
	if err := os.Remove(address); err != nil && !os.IsNotExist(err) {
		return errors.Wrap(err, "error cleaning up previous kubelet socket")
	}

	k := &fakeKubelet{
		vnetPluginRegisterChan: vnetPluginRegisterChan,
		ibPluginRegisterChan:   ibPluginRegisterChan,
		pluginPrefix:           pluginPrefix,
	}
	grpcServer := grpc.NewServer()
	v1beta1.RegisterRegistrationServer(grpcServer, k)

	l, err := net.Listen("unix", address)
	if err != nil {
		return errors.Wrap(err, "error from fake kubelet listening on socket")
	}
	errChan := make(chan error, 2)
	go func() {
		errChan <- grpcServer.Serve(l)
	}()
	defer grpcServer.Stop()

	select {
	case err := <-errChan:
		return errors.Wrap(err, "error running fake kubelet grpc server")
	case <-ctx.Done():
	}
	return nil
}

func getDeviceCount(t *testing.T, pluginAddress string) int {
	conn, err := grpc.Dial(pluginAddress, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			d := &net.Dialer{}
			return d.DialContext(ctx, "unix", addr)
		}))
	if err != nil {
		t.Fatalf("error connecting to fake kubelet: %v", err)
	}
	defer conn.Close()

	client := v1beta1.NewDevicePluginClient(conn)
	lwClient, err := client.ListAndWatch(context.Background(), &v1beta1.Empty{})
	if err != nil {
		t.Fatalf("error from listAndWatch: %v", err)
	}

	resp, err := lwClient.Recv()
	if err != nil {
		t.Fatalf("error from listAndWatch Recv: %v", err)
	}

	return len(resp.Devices)
}
