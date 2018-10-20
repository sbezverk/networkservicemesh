// Copyright (c) 2018 Cisco and/or its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/ligato/networkservicemesh/pkg/nsm/apis/common"
	"github.com/ligato/networkservicemesh/pkg/nsm/apis/netmesh"
	"github.com/ligato/networkservicemesh/pkg/nsm/apis/nseconnect"
	"github.com/ligato/networkservicemesh/pkg/tools"
	"github.com/sirupsen/logrus"
	"google.golang.org/grpc"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// networkServiceName defines Network Service Name the NSE is serving for
	networkServiceName = "gold-network"
	// EndpointSocketBaseDir defines the location of NSM Endpoints listen socket
	EndpointSocketBaseDir = "/var/lib/networkservicemesh"
	// EndpointSocket defines the name of NSM Endpoints operations socket
	EndpointSocket = "nsm.endpoint.io.sock"

	// NSMkeyNSELocalMechanism defines a key for nse local interface
	// TODO (sbezverk) must be known globally
	NSMkeyNSELocalMechanism = "nselocalmechanism"
)

var (
	kubeconfig = flag.String("kubeconfig", "", "Absolute path to the kubeconfig file. Either this or master needs to be set if the provisioner is being run out of cluster.")
)

type nseConnection struct {
	podUID             string
	networkServiceName string
	linuxNamespace     string
}

func (n nseConnection) RequestEndpointConnection(ctx context.Context, req *nseconnect.EndpointConnectionRequest) (*nseconnect.EndpointConnectionReply, error) {

	logrus.Infof("Received RequestEndpointConnection for id: %s Network Service: %s", req.RequestId, req.NetworkServiceName)

	return &nseconnect.EndpointConnectionReply{
		RequestId:          n.podUID,
		NetworkServiceName: n.networkServiceName,
		LinuxNamespace:     n.linuxNamespace,
	}, nil
}

func (n nseConnection) SendEndpointConnectionMechanism(ctx context.Context, req *nseconnect.EndpointConnectionMechanism) (*nseconnect.EndpointConnectionMechanismReply, error) {

	logrus.Infof("Received SendEndpointConnectionMechanism for id: %s Network Service: %s local mechanism: %+v", req.RequestId, req.NetworkServiceName, req.LocalMechanism)

	interfaceName, ok := req.LocalMechanism.Parameters[NSMkeyNSELocalMechanism]
	if !ok {
		logrus.Errorf("Missing nselocalmechansim key in parameters")
		return &nseconnect.EndpointConnectionMechanismReply{
			RequestId:      req.RequestId,
			MechanismFound: false,
		}, fmt.Errorf("Missing nselocalmechansim key in parameters")
	}
	logrus.Infof("Got notification to use interface: %s for network service %s for request id %s", interfaceName, req.NetworkServiceName, req.RequestId)

	interfaces, err := net.Interfaces()
	if err != nil {
		logrus.Errorf("Failed to lists interfaces with error: %+v", err)
		return &nseconnect.EndpointConnectionMechanismReply{
			RequestId:      req.RequestId,
			MechanismFound: false,
		}, fmt.Errorf("Failed to lists interfaces with error: %+v", err)
	}

	// Checking if the interface created by NSM/Dataplane is found in the list
	logrus.Info("List of interfaces:")
	for _, intfs := range interfaces {
		logrus.Infof("- %s", strings.ToLower(intfs.Name))
		if strings.ToLower(intfs.Name) == strings.ToLower(interfaceName) {
			return &nseconnect.EndpointConnectionMechanismReply{
				RequestId:      req.RequestId,
				MechanismFound: true,
			}, nil
		}
	}
	// Interface has not been found in the pos'd net namespace, returning failure to NSM
	return &nseconnect.EndpointConnectionMechanismReply{
		RequestId:      req.RequestId,
		MechanismFound: false,
	}, fmt.Errorf("Interface %s is not found", interfaceName)
}

func buildClient() (*kubernetes.Clientset, error) {
	var config *rest.Config
	var err error

	kubeconfigEnv := os.Getenv("KUBECONFIG")

	if kubeconfigEnv != "" {
		kubeconfig = &kubeconfigEnv
	}

	if *kubeconfig != "" {
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
	} else {
		config, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, err
	}
	k8s, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return k8s, nil
}

func main() {
	// TODO (sbezverk) migtate to cobra package for flags and arguments
	flag.Parse()
	var wg sync.WaitGroup

	k8s, err := buildClient()
	if err != nil {
		logrus.Errorf("nse: fail to build kubernetes client with error: %+v, exiting...", err)
		os.Exit(1)
	}
	namespace := os.Getenv("INIT_NAMESPACE")
	if namespace == "" {
		logrus.Error("nse: cannot detect namespace, make sure INIT_NAMESPACE variable is set via downward api, exiting...")
		os.Exit(1)
	}
	podName := os.Getenv("HOSTNAME")

	// podUID is used as a unique identifier for nse init process, it will stay the same throughout life of
	// pod and will guarantee idempotency of possible repeated requests to NSM
	pod, err := k8s.CoreV1().Pods(namespace).Get(podName, metav1.GetOptions{})
	if err != nil {
		logrus.Errorf("nse: failure to get pod  %s/%s with error: %+v, exiting...", namespace, podName, err)
		os.Exit(1)
	}
	podUID := string(pod.GetUID())

	// For NSE to program container's dataplane, container's linux namespace must be sent to NSM
	linuxNS, err := tools.GetCurrentNS()
	if err != nil {
		logrus.Fatalf("nse: failed to get a linux namespace for pod %s/%s with error: %+v, exiting...", namespace, podName, err)
		os.Exit(1)
	}

	// NSM socket path will be used to drop NSE socket for NSM's Connection request
	connectionServerSocket := path.Join(EndpointSocketBaseDir, podUID+".nse.io.sock")
	if err := tools.SocketCleanup(connectionServerSocket); err != nil {
		logrus.Fatalf("nse: failure to cleanup stale socket %s with error: %+v", connectionServerSocket, err)
	}

	logrus.Infof("nse: listening socket %s", connectionServerSocket)
	connectionServer, err := net.Listen("unix", connectionServerSocket)
	if err != nil {
		logrus.Fatalf("nse: failure to listen on a socket %s with error: %+v", connectionServerSocket, err)
	}
	grpcServer := grpc.NewServer()

	// Registering NSE API, it will listen for Connection requests from NSM and return information
	// needed for NSE's dataplane programming.
	nseConn := nseConnection{
		podUID:             podUID,
		networkServiceName: networkServiceName,
		linuxNamespace:     linuxNS,
	}

	nseconnect.RegisterEndpointConnectionServer(grpcServer, nseConn)
	go func() {
		wg.Add(1)
		if err := grpcServer.Serve(connectionServer); err != nil {
			logrus.Fatalf("nse: failed to start grpc server on socket %s with error: %+v ", connectionServerSocket, err)
		}
	}()
	// Check if the socket of Endpoint Connection Server is operation
	testSocket, err := tools.SocketOperationCheck(connectionServerSocket)
	if err != nil {
		logrus.Fatalf("nse: failure to communicate with the socket %s with error: %+v", connectionServerSocket, err)
	}
	testSocket.Close()

	// NSE connection server is ready and now endpoints can be advertised to NSM
	advertiseSocket := path.Join(EndpointSocketBaseDir, EndpointSocket)

	if _, err := os.Stat(advertiseSocket); err != nil {
		logrus.Errorf("nse: failure to access nsm socket at %s with error: %+v, exiting...", advertiseSocket, err)
		os.Exit(1)
	}

	conn, err := tools.SocketOperationCheck(advertiseSocket)
	if err != nil {
		logrus.Fatalf("nse: failure to communicate with the socket %s with error: %+v", advertiseSocket, err)
	}
	defer conn.Close()
	logrus.Infof("nsm: connection to nsm server on socket: %s succeeded.", advertiseSocket)

	advertieConnection := nseconnect.NewEndpointOperationsClient(conn)

	endpoint := netmesh.NetworkServiceEndpoint{
		NseProviderName:    podUID,
		NetworkServiceName: networkServiceName,
		SocketLocation:     connectionServerSocket,
		LocalMechanisms: []*common.LocalMechanism{
			{
				Type: common.LocalMechanismType_KERNEL_INTERFACE,
			},
		},
	}
	resp, err := advertieConnection.AdvertiseEndpoint(context.Background(), &nseconnect.EndpointAdvertiseRequest{
		RequestId:       podUID,
		NetworkEndpoint: &endpoint,
	})
	if err != nil {
		grpcServer.Stop()
		logrus.Fatalf("nse: failure to communicate with the socket %s with error: %+v", advertiseSocket, err)

	}
	if !resp.Accepted {
		grpcServer.Stop()
		logrus.Fatalf("nse: NSM response is inidcating failure of accepting endpoint Advertisiment.")
	}

	logrus.Infof("nse: channel has been successfully advertised, waiting for connection from NSM...")
	// Now block on WaitGroup
	wg.Wait()
}
