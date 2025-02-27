/*
Copyright 2023 The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vgpu

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/NVIDIA/go-gpuallocator/gpuallocator"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/klog/v2"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	"volcano.sh/k8s-device-plugin/pkg/lock"
	"volcano.sh/k8s-device-plugin/pkg/plugin/vgpu/config"
	"volcano.sh/k8s-device-plugin/pkg/plugin/vgpu/util"
)

// Constants to represent the various device list strategies
const (
	DeviceListStrategyEnvvar       = "envvar"
	DeviceListStrategyVolumeMounts = "volume-mounts"
)

// Constants to represent the various device id strategies
const (
	DeviceIDStrategyUUID  = "uuid"
	DeviceIDStrategyIndex = "index"
)

// Constants for use by the 'volume-mounts' device list strategy
const (
	deviceListAsVolumeMountsHostPath          = "/dev/null"
	deviceListAsVolumeMountsContainerPathRoot = "/var/run/nvidia-container-devices"
)

// NvidiaDevicePlugin implements the Kubernetes device plugin API
type NvidiaDevicePlugin struct {
	ResourceManager
	deviceCache        *DeviceCache
	resourceName       string
	deviceListEnvvar   string
	deviceListStrategy string
	allocatePolicy     gpuallocator.Policy
	socket             string

	server        *grpc.Server
	cachedDevices []*Device
	health        chan *Device
	stop          chan interface{}
	changed       chan struct{}
	migStrategy   string
}

// NewNvidiaDevicePlugin returns an initialized NvidiaDevicePlugin
func NewNvidiaDevicePlugin(resourceName string, deviceCache *DeviceCache, allocatePolicy gpuallocator.Policy, socket string,
	deviceListEnvvar string, deviceListStrategy string,
) *NvidiaDevicePlugin {
	return &NvidiaDevicePlugin{
		deviceCache:        deviceCache,
		resourceName:       resourceName,
		allocatePolicy:     allocatePolicy,
		socket:             socket,
		migStrategy:        "none",
		deviceListEnvvar:   deviceListEnvvar,
		deviceListStrategy: deviceListStrategy,

		// These will be reinitialized every
		// time the plugin server is restarted.
		server: nil,
		health: nil,
		stop:   nil,
	}
}

// NewNvidiaDevicePlugin returns an initialized NvidiaDevicePlugin
func NewMIGNvidiaDevicePlugin(resourceName string, resourceManager ResourceManager, allocatePolicy gpuallocator.Policy, socket string,
	deviceListEnvvar string, deviceListStrategy string,
) *NvidiaDevicePlugin {
	return &NvidiaDevicePlugin{
		ResourceManager:    resourceManager,
		resourceName:       resourceName,
		deviceListEnvvar:   deviceListEnvvar,
		deviceListStrategy: deviceListStrategy,
		allocatePolicy:     allocatePolicy,
		socket:             socket,

		// These will be reinitialized every
		// time the plugin server is restarted.
		cachedDevices: nil,
		server:        nil,
		health:        nil,
		stop:          nil,
		migStrategy:   "mixed",
	}
}

func (m *NvidiaDevicePlugin) initialize() {
	var err error
	if strings.Compare(m.migStrategy, "mixed") == 0 {
		m.cachedDevices = m.ResourceManager.Devices()
	}
	m.server = grpc.NewServer([]grpc.ServerOption{}...)
	m.health = make(chan *Device)
	m.stop = make(chan interface{})
	check(err)
}

func (m *NvidiaDevicePlugin) cleanup() {
	close(m.stop)
	m.server = nil
	m.health = nil
	m.stop = nil
}

// Start starts the gRPC server, registers the device plugin with the Kubelet,
// and starts the device healthchecks.
func (m *NvidiaDevicePlugin) Start() error {
	m.initialize()

	err := m.Serve()
	if err != nil {
		log.Printf("Could not start device plugin for '%s': %s", m.resourceName, err)
		m.cleanup()
		return err
	}
	log.Printf("Starting to serve '%s' on %s", m.resourceName, m.socket)

	err = m.Register()
	if err != nil {
		log.Printf("Could not register device plugin: %s", err)
		m.Stop()
		return err
	}
	log.Printf("Registered device plugin for '%s' with Kubelet", m.resourceName)

	if strings.Compare(m.migStrategy, "none") == 0 {
		m.deviceCache.AddNotifyChannel("plugin", m.health)
	} else if strings.Compare(m.migStrategy, "mixed") == 0 {
		go m.CheckHealth(m.stop, m.cachedDevices, m.health)
	} else {
		log.Panicln("migstrategy not recognized", m.migStrategy)
	}
	return nil
}

// Stop stops the gRPC server.
func (m *NvidiaDevicePlugin) Stop() error {
	if m == nil || m.server == nil {
		return nil
	}
	log.Printf("Stopping to serve '%s' on %s", m.resourceName, m.socket)
	m.deviceCache.RemoveNotifyChannel("plugin")
	m.server.Stop()
	if err := os.Remove(m.socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	m.cleanup()
	return nil
}

// Serve starts the gRPC server of the device plugin.
func (m *NvidiaDevicePlugin) Serve() error {
	os.Remove(m.socket)
	sock, err := net.Listen("unix", m.socket)
	if err != nil {
		return err
	}

	pluginapi.RegisterDevicePluginServer(m.server, m)

	go func() {
		lastCrashTime := time.Now()
		restartCount := 0
		for {
			log.Printf("Starting GRPC server for '%s'", m.resourceName)
			err := m.server.Serve(sock)
			if err == nil {
				break
			}

			log.Printf("GRPC server for '%s' crashed with error: %v", m.resourceName, err)

			// restart if it has not been too often
			// i.e. if server has crashed more than 5 times and it didn't last more than one hour each time
			if restartCount > 5 {
				// quit
				log.Fatalf("GRPC server for '%s' has repeatedly crashed recently. Quitting", m.resourceName)
			}
			timeSinceLastCrash := time.Since(lastCrashTime).Seconds()
			lastCrashTime = time.Now()
			if timeSinceLastCrash > 3600 {
				// it has been one hour since the last crash.. reset the count
				// to reflect on the frequency
				restartCount = 1
			} else {
				restartCount++
			}
		}
	}()

	// Wait for server to start by launching a blocking connexion
	conn, err := m.dial(m.socket, 5*time.Second)
	if err != nil {
		return err
	}
	conn.Close()

	return nil
}

// Register registers the device plugin for the given resourceName with Kubelet.
func (m *NvidiaDevicePlugin) Register() error {
	conn, err := m.dial(pluginapi.KubeletSocket, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pluginapi.NewRegistrationClient(conn)
	reqt := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     path.Base(m.socket),
		ResourceName: m.resourceName,
		Options:      &pluginapi.DevicePluginOptions{},
	}

	_, err = client.Register(context.Background(), reqt)
	if err != nil {
		return err
	}
	return nil
}

// GetDevicePluginOptions returns the values of the optional settings for this plugin
func (m *NvidiaDevicePlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	options := &pluginapi.DevicePluginOptions{}
	return options, nil
}

// ListAndWatch lists devices and update that list according to the health status
func (m *NvidiaDevicePlugin) ListAndWatch(e *pluginapi.Empty, s pluginapi.DevicePlugin_ListAndWatchServer) error {
	_ = s.Send(&pluginapi.ListAndWatchResponse{Devices: m.apiDevices()})
	for {
		select {
		case <-m.stop:
			return nil
		case d := <-m.health:
			// FIXME: there is no way to recover from the Unhealthy state.
			// d.Health = pluginapi.Unhealthy
			log.Printf("'%s' device marked unhealthy: %s", m.resourceName, d.ID)
			_ = s.Send(&pluginapi.ListAndWatchResponse{Devices: m.apiDevices()})
		}
	}
}

func (m *NvidiaDevicePlugin) MIGAllocate(ctx context.Context, reqs *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	responses := pluginapi.AllocateResponse{}
	for _, req := range reqs.ContainerRequests {
		for _, id := range req.DevicesIDs {
			if !m.deviceExists(id) {
				return nil, fmt.Errorf("invalid allocation request for '%s': unknown device: %s", m.resourceName, id)
			}
		}

		response := pluginapi.ContainerAllocateResponse{}

		uuids := req.DevicesIDs
		deviceIDs := m.deviceIDsFromUUIDs(uuids)

		response.Envs = m.apiEnvs(m.deviceListEnvvar, deviceIDs)

		klog.Infof("response=", response.Envs)
		responses.ContainerResponses = append(responses.ContainerResponses, &response)
	}

	return &responses, nil
}

// Allocate which return list of devices.
func (m *NvidiaDevicePlugin) Allocate(ctx context.Context, reqs *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	klog.Infoln("Allocate", reqs.ContainerRequests)
	if len(reqs.ContainerRequests) > 1 {
		return &pluginapi.AllocateResponse{}, errors.New("multiple Container Requests not supported")
	}
	if strings.Compare(m.migStrategy, "mixed") == 0 {
		return m.MIGAllocate(ctx, reqs)
	}
	responses := pluginapi.AllocateResponse{}
	nodename := os.Getenv("NODE_NAME")

	current, err := util.GetPendingPod(nodename)
	if err != nil {
		lock.ReleaseNodeLock(nodename, util.VGPUDeviceName)
		return &pluginapi.AllocateResponse{}, err
	}

	for idx := range reqs.ContainerRequests {
		currentCtr, devreq, err := util.GetNextDeviceRequest(util.NvidiaGPUDevice, *current)
		klog.Infoln("deviceAllocateFromAnnotation=", devreq)
		if err != nil {
			klog.Errorln("get device from annotation failed", err.Error())
			util.PodAllocationFailed(nodename, current)
			return &pluginapi.AllocateResponse{}, err
		}
		if len(devreq) != len(reqs.ContainerRequests[idx].DevicesIDs) {
			klog.Errorln("device number not matched", devreq, reqs.ContainerRequests[idx].DevicesIDs)
			util.PodAllocationFailed(nodename, current)
			return &pluginapi.AllocateResponse{}, errors.New("device number not matched")
		}

		err = util.EraseNextDeviceTypeFromAnnotation(util.NvidiaGPUDevice, *current)
		if err != nil {
			klog.Errorln("Erase annotation failed", err.Error())
			util.PodAllocationFailed(nodename, current)
			return &pluginapi.AllocateResponse{}, err
		}

		response := pluginapi.ContainerAllocateResponse{}
		response.Envs = make(map[string]string)
		for i, dev := range devreq {
			limitKey := fmt.Sprintf("CUDA_DEVICE_MEMORY_LIMIT_%v", i)
			response.Envs[limitKey] = fmt.Sprintf("%vm", dev.Usedmem)
			m.addVisibleDevices(&response, dev.UUID)
		}
		response.Envs["CUDA_DEVICE_MEMORY_SHARED_CACHE"] = fmt.Sprintf("/tmp/vgpu/%v.cache", uuid.NewUUID())

		cacheFileHostDirectory := "/tmp/vgpu/containers/" + string(current.UID) + "_" + currentCtr.Name
		os.MkdirAll(cacheFileHostDirectory, 0o777)
		os.Chmod(cacheFileHostDirectory, 0o777)
		os.MkdirAll("/tmp/vgpulock", 0o777)
		os.Chmod("/tmp/vgpulock", 0o777)
		hostHookPath := os.Getenv("HOOK_PATH")
		response.Mounts = append(response.Mounts,
			&pluginapi.Mount{
				ContainerPath: "/usr/local/vgpu/libvgpu.so",
				HostPath:      hostHookPath + "/libvgpu.so",
				ReadOnly:      true,
			},
			&pluginapi.Mount{
				ContainerPath: "/etc/ld.so.preload",
				HostPath:      hostHookPath + "/ld.so.preload",
				ReadOnly:      true,
			},
			&pluginapi.Mount{
				ContainerPath: "/tmp/vgpu",
				HostPath:      cacheFileHostDirectory,
				ReadOnly:      false,
			},
			&pluginapi.Mount{
				ContainerPath: "/tmp/vgpulock",
				HostPath:      "/tmp/vgpulock",
				ReadOnly:      false,
			},
		)
		responses.ContainerResponses = append(responses.ContainerResponses, &response)
	}
	klog.Infoln("Allocate Response", responses.ContainerResponses)
	util.PodAllocationTrySuccess(nodename, current)
	return &responses, nil
}

func (m *NvidiaDevicePlugin) addVisibleDevices(response *pluginapi.ContainerAllocateResponse, deviceID string) {
	switch m.deviceListStrategy {
	case DeviceListStrategyVolumeMounts:
		response.Envs[m.deviceListEnvvar] = deviceListAsVolumeMountsContainerPathRoot
		response.Mounts = append(response.Mounts, &pluginapi.Mount{
			HostPath:      deviceListAsVolumeMountsHostPath,
			ContainerPath: filepath.Join(deviceListAsVolumeMountsContainerPathRoot, deviceID),
		})
	case DeviceListStrategyEnvvar:
		// append to existing value
		if exists := response.Envs[m.deviceListEnvvar]; len(exists) == 0 {
			response.Envs[m.deviceListEnvvar] = deviceID
		} else {
			response.Envs[m.deviceListEnvvar] = fmt.Sprintf("%s,%s", exists, deviceID)
		}
	}
}

// PreStartContainer is unimplemented for this plugin
func (m *NvidiaDevicePlugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

// dial establishes the gRPC communication with the registered device plugin.
func (m *NvidiaDevicePlugin) dial(unixSocketPath string, timeout time.Duration) (*grpc.ClientConn, error) {
	c, err := grpc.Dial(unixSocketPath, grpc.WithInsecure(), grpc.WithBlock(),
		grpc.WithTimeout(timeout),
		grpc.WithDialer(func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}),
	)
	if err != nil {
		return nil, err
	}

	return c, nil
}

func (m *NvidiaDevicePlugin) Devices() []*Device {
	if strings.Compare(m.migStrategy, "none") == 0 {
		return m.deviceCache.GetCache()
	}
	if strings.Compare(m.migStrategy, "mixed") == 0 {
		return m.ResourceManager.Devices()
	}
	log.Panic("migStrategy not recognized,exiting...")
	return []*Device{}
}

func (m *NvidiaDevicePlugin) deviceExists(id string) bool {
	for _, d := range m.cachedDevices {
		if d.ID == id {
			return true
		}
	}
	return false
}

func (m *NvidiaDevicePlugin) deviceIDsFromUUIDs(uuids []string) []string {
	return uuids
}

func (m *NvidiaDevicePlugin) apiDevices() []*pluginapi.Device {
	if strings.Compare(m.migStrategy, "mixed") == 0 {
		var pdevs []*pluginapi.Device
		for _, d := range m.cachedDevices {
			pdevs = append(pdevs, &d.Device)
		}
		return pdevs
	}
	devices := m.Devices()
	var res []*pluginapi.Device
	for _, dev := range devices {
		for i := uint(0); i < config.DeviceSplitCount; i++ {
			id := fmt.Sprintf("%v-%v", dev.ID, i)
			res = append(res, &pluginapi.Device{
				ID:       id,
				Health:   dev.Health,
				Topology: nil,
			})
		}
	}
	return res
}

func (m *NvidiaDevicePlugin) apiEnvs(envvar string, deviceIDs []string) map[string]string {
	return map[string]string{
		envvar: strings.Join(deviceIDs, ","),
	}
}
