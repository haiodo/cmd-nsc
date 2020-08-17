// Copyright (c) 2020 Doc.ai and/or its affiliates.
//
// SPDX-License-Identifier: Apache-2.0
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

// Package main define a nsc application
package main

import (
	"context"
	"fmt"
	"os"
	"path"
	"runtime"
	"strconv"
	"time"

	"github.com/networkservicemesh/api/pkg/api/networkservice"
	"github.com/networkservicemesh/api/pkg/api/networkservice/mechanisms/cls"
	"github.com/networkservicemesh/api/pkg/api/networkservice/mechanisms/common"
	"github.com/networkservicemesh/api/pkg/api/networkservice/mechanisms/kernel"
	"github.com/networkservicemesh/api/pkg/api/networkservice/mechanisms/memif"
	"github.com/networkservicemesh/cmd-nsc/pkg/config"
	"github.com/networkservicemesh/sdk/pkg/networkservice/chains/client"
	"github.com/networkservicemesh/sdk/pkg/tools/fs"
	"github.com/networkservicemesh/sdk/pkg/tools/grpcutils"
	"github.com/networkservicemesh/sdk/pkg/tools/spiffejwt"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	nested "github.com/antonfisher/nested-logrus-formatter"
	"github.com/kelseyhightower/envconfig"
	"github.com/networkservicemesh/sdk/pkg/tools/jaeger"
	"github.com/networkservicemesh/sdk/pkg/tools/log"
	"github.com/networkservicemesh/sdk/pkg/tools/signalctx"
	"github.com/networkservicemesh/sdk/pkg/tools/spanhelper"
	"github.com/opentracing/opentracing-go"
	"github.com/sirupsen/logrus"
)

func main() {
	// ********************************************************************************
	// Configure signal handling context
	// ********************************************************************************
	ctx := signalctx.WithSignals(context.Background())
	var cancel context.CancelFunc
	ctx, cancel = context.WithCancel(ctx)
	defer cancel()

	// ********************************************************************************
	// Setup logger
	// ********************************************************************************
	logrus.Info("Starting NetworkServiceMesh Client ...")
	logrus.SetFormatter(&nested.Formatter{})
	logrus.SetLevel(logrus.TraceLevel)

	ctx = log.WithField(ctx, "cmd", os.Args[:1])

	// ********************************************************************************
	// Configure open tracing
	// ********************************************************************************
	var span opentracing.Span
	// Enable Jaeger
	if jaeger.IsOpentracingEnabled() {
		jaegerCloser := jaeger.InitJaeger("nsc")
		defer func() { _ = jaegerCloser.Close() }()
		span = opentracing.StartSpan("nsc")
	}
	cmdSpan := spanhelper.NewSpanHelper(ctx, span, "nsc")

	// ********************************************************************************
	// Get config from environment
	// ********************************************************************************
	rootConf := &config.Config{}
	if err := envconfig.Usage("nsm", rootConf); err != nil {
		logrus.Fatal(err)
	}
	if err := envconfig.Process("nsm", rootConf); err != nil {
		logrus.Fatalf("error processing rootConf from env: %+v", err)
	}

	nsmClient := NewNsmClient(ctx, rootConf)
	connections, err := RunClient(cmdSpan.Context(), rootConf, nsmClient)
	if err != nil {
		logrus.Errorf("failed to connect to network services")
	} else {
		logrus.Infof("All client init operations are done.")
	}

	// Startup is finished
	cmdSpan.Finish()

	// Wait for cancel event to terminate
	<-ctx.Done()

	logrus.Infof("Performing cleanup of connections due terminate...")
	for _, c := range connections {
		_, err := nsmClient.Close(context.Background(), c)
		if err != nil {
			logrus.Infof("Failed to close connection %v cause: %v", c, err)
		}
	}
}

// NewNsmClient - creates a client connection to NSMGr
func NewNsmClient(ctx context.Context, rootConf *config.Config) networkservice.NetworkServiceClient {
	// ********************************************************************************
	// Get a x509Source
	// ********************************************************************************
	source, err := workloadapi.NewX509Source(ctx)
	if err != nil {
		logrus.Fatalf("error getting x509 source: %+v", err)
	}
	var svid *x509svid.SVID
	svid, err = source.GetX509SVID()
	if err != nil {
		logrus.Fatalf("error getting x509 svid: %+v", err)
	}
	logrus.Infof("sVID: %q", svid.ID)

	// ********************************************************************************
	// Connect to NSManager
	// ********************************************************************************
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	logrus.Infof("NSC: Connecting to Network Service Manager %v", rootConf.ConnectTo.String())
	var clientCC *grpc.ClientConn
	clientCC, err = grpc.DialContext(ctx,
		grpcutils.URLToTarget(rootConf.ConnectTo),
		grpc.WithTransportCredentials(
			credentials.NewTLS(
				tlsconfig.MTLSClientConfig(source, source, tlsconfig.AuthorizeAny()))),
		grpc.WithDefaultCallOptions(
			grpc.WaitForReady(true)))
	if err != nil {
		logrus.Fatalf("failed to dial NSM: %v", err)
	}

	// ********************************************************************************
	// Create Network Service Manager nsmClient
	// ********************************************************************************
	return client.NewClient(ctx, rootConf.Name, nil, spiffejwt.TokenGeneratorFunc(source, rootConf.MaxTokenLifetime), clientCC)
}

func RunClient(ctx context.Context, rootConf *config.Config, nsmClient networkservice.NetworkServiceClient) ([]*networkservice.Connection, error) {

	// Validate config parameters
	if err := rootConf.IsValid(); err != nil {
		return []*networkservice.Connection{}, err
	}

	// ********************************************************************************
	// Initiate connections
	// ********************************************************************************

	// A list of cleanup operations
	connections := []*networkservice.Connection{}

	for idx, clientConf := range rootConf.NetworkServices {
		err := clientConf.MergeWithConfigOptions(rootConf)
		if err != nil {
			logrus.Errorf("error during nsmClient config aggregation %v", err)
			return connections, err
		}
		// We need update
		outgoingMechanism := &networkservice.Mechanism{
			Cls:        cls.LOCAL,
			Type:       clientConf.Mechanism,
			Parameters: map[string]string{},
		}

		switch clientConf.Mechanism {
		case kernel.MECHANISM:
			outgoingMechanism.Parameters[common.InterfaceNameKey] = clientConf.Path[0]

			logrus.Infof("%v", runtime.GOOS)
			if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
				// Check we are not macos or windows.
				inode, err := fs.GetInode("/proc/self/ns/net")
				if err != nil {
					logrus.Errorf("could not retrieve a linux namespace %v", err)
					return connections, err
				}
				outgoingMechanism.Parameters[kernel.NetNSInodeKey] = strconv.FormatUint(uint64(inode), 10)
			}

			kernel.ToMechanism(outgoingMechanism).SetNetNSURL("unix:///proc/self/ns/net")

		case memif.MECHANISM:
			outgoingMechanism.Parameters[memif.SocketFilename] = path.Join(clientConf.Path...)
		}

		// Construct a request
		request := &networkservice.NetworkServiceRequest{
			Connection: &networkservice.Connection{
				Id: fmt.Sprintf("%s-%d", rootConf.Name, idx),
			},
			MechanismPreferences: []*networkservice.Mechanism{
				outgoingMechanism,
			},
		}

		// Performing nsmClient connection request
		conn, connerr := nsmClient.Request(ctx, request)
		if connerr != nil {
			logrus.Errorf("Failed to request network service with %v: err %v", request, connerr)
			return connections, connerr
		}

		logrus.Infof("Network service established with %v\n Connection:%v", request, conn)

		// Add connection to list
		connections = append(connections, conn)
	}
	return connections, nil
}
