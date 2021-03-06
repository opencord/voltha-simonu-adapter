/*
 * Copyright 2018-present Open Networking Foundation

 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at

 * http://www.apache.org/licenses/LICENSE-2.0

 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/opencord/voltha-lib-go/v3/pkg/adapters"
	com "github.com/opencord/voltha-lib-go/v3/pkg/adapters/common"
	"github.com/opencord/voltha-lib-go/v3/pkg/db/kvstore"
	"github.com/opencord/voltha-lib-go/v3/pkg/kafka"
	"github.com/opencord/voltha-lib-go/v3/pkg/log"
	"github.com/opencord/voltha-lib-go/v3/pkg/probe"
	"github.com/opencord/voltha-lib-go/v3/pkg/version"
	ic "github.com/opencord/voltha-protos/v3/go/inter_container"
	"github.com/opencord/voltha-protos/v3/go/voltha"
	ac "github.com/opencord/voltha-simonu-adapter/internal/pkg/adaptercore"
	"github.com/opencord/voltha-simonu-adapter/internal/pkg/config"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

type adapter struct {
	instanceId       string
	config           *config.AdapterFlags
	iAdapter         adapters.IAdapter
	kafkaClient      kafka.Client
	kvClient         kvstore.Client
	kip              kafka.InterContainerProxy
	coreProxy        *com.CoreProxy
	halted           bool
	exitChannel      chan int
	receiverChannels []<-chan *ic.InterContainerMessage
}

func init() {
	log.AddPackage(log.JSON, log.DebugLevel, nil)
}

func newAdapter(cf *config.AdapterFlags) *adapter {
	var a adapter
	a.instanceId = cf.InstanceID
	a.config = cf
	a.halted = false
	a.exitChannel = make(chan int, 1)
	a.receiverChannels = make([]<-chan *ic.InterContainerMessage, 0)
	return &a
}

func (a *adapter) start(ctx context.Context) {
	log.Info("Starting Core Adapter components")
	var err error

	// If the context has a probe then fetch it and register our services
	var p *probe.Probe
	if value := ctx.Value(probe.ProbeContextKey); value != nil {
		if _, ok := value.(*probe.Probe); ok {
			p = value.(*probe.Probe)
			p.RegisterService(
				"message-bus",
				"kv-store",
				"container-proxy",
				"core-request-handler",
				"register-with-core",
			)
		}
	}

	// Setup KV Client
	log.Debugw("create-kv-client", log.Fields{"kvstore": a.config.KVStoreType})
	if err := a.setKVClient(); err != nil {
		log.Fatal("error-setting-kv-client")
	}

	if p != nil {
		p.UpdateStatus("kv-store", probe.ServiceStatusRunning)
	}

	// Setup Kafka Client
	if a.kafkaClient, err = newKafkaClient("sarama", a.config.KafkaAdapterHost, a.config.KafkaAdapterPort); err != nil {
		log.Fatal("Unsupported-common-client")
	}

	if p != nil {
		p.UpdateStatus("message-bus", probe.ServiceStatusRunning)
	}

	// Start the common InterContainer Proxy - retry indefinitely
	if a.kip, err = a.startInterContainerProxy(ctx, -1); err != nil {
		log.Fatal("error-starting-inter-container-proxy")
	}

	// Create the core proxy to handle requests to the Core
	a.coreProxy = com.NewCoreProxy(a.kip, a.config.Topic, a.config.CoreTopic)

	// Create the simulated OLT adapter
	if a.iAdapter, err = a.startSimulatedONU(ctx, a.kip, a.coreProxy); err != nil {
		log.Fatal("error-starting-inter-container-proxy")
	}

	// Register the core request handler
	if err = a.setupRequestHandler(ctx, a.instanceId, a.iAdapter, a.coreProxy); err != nil {
		log.Fatal("error-setting-core-request-handler")
	}

	//	Register this adapter to the Core - retry indefinitely
	if err = a.registerWithCore(ctx, -1); err != nil {
		log.Fatal("error-registering-with-core")
	}
}

func (rw *adapter) stop(ctx context.Context) {
	// Stop leadership tracking
	rw.halted = true

	// send exit signal
	rw.exitChannel <- 0

	// Cleanup - applies only if we had a kvClient
	if rw.kvClient != nil {
		// Release all reservations
		if err := rw.kvClient.ReleaseAllReservations(ctx); err != nil {
			log.Infow("fail-to-release-all-reservations", log.Fields{"error": err})
		}
		// Close the DB connection
		rw.kvClient.Close()
	}

	// TODO:  More cleanup
}

func newKVClient(storeType string, address string, timeout int) (kvstore.Client, error) {

	log.Infow("kv-store-type", log.Fields{"store": storeType})
	switch storeType {
	case "consul":
		return kvstore.NewConsulClient(address, timeout)
	case "etcd":
		return kvstore.NewEtcdClient(address, timeout)
	}
	return nil, errors.New("unsupported-kv-store")
}

func newKafkaClient(clientType string, host string, port int) (kafka.Client, error) {

	log.Infow("common-client-type", log.Fields{"client": clientType})
	switch clientType {
	case "sarama":
		return kafka.NewSaramaClient(
			kafka.Host(host),
			kafka.Port(port),
			kafka.ProducerReturnOnErrors(true),
			kafka.ProducerReturnOnSuccess(true),
			kafka.ProducerMaxRetries(6),
			kafka.ProducerRetryBackoff(time.Millisecond*30)), nil
	}
	return nil, errors.New("unsupported-client-type")
}

func (a *adapter) setKVClient() error {
	addr := a.config.KVStoreHost + ":" + strconv.Itoa(a.config.KVStorePort)
	client, err := newKVClient(a.config.KVStoreType, addr, a.config.KVStoreTimeout)
	if err != nil {
		a.kvClient = nil
		log.Error(err)
		return err
	}
	a.kvClient = client
	return nil
}

func toString(value interface{}) (string, error) {
	switch t := value.(type) {
	case []byte:
		return string(value.([]byte)), nil
	case string:
		return value.(string), nil
	default:
		return "", fmt.Errorf("unexpected-type-%T", t)
	}
}

func (a *adapter) startInterContainerProxy(ctx context.Context, retries int) (kafka.InterContainerProxy, error) {
	log.Infow("starting-intercontainer-messaging-proxy", log.Fields{"host": a.config.KafkaAdapterHost,
		"port": a.config.KafkaAdapterPort, "topic": a.config.Topic})
	var err error
	kip := kafka.NewInterContainerProxy(
		kafka.InterContainerHost(a.config.KafkaAdapterHost),
		kafka.InterContainerPort(a.config.KafkaAdapterPort),
		kafka.MsgClient(a.kafkaClient),
		kafka.DefaultTopic(&kafka.Topic{Name: a.config.Topic}))
	count := 0
	for {
		if err = kip.Start(); err != nil {
			log.Warnw("error-starting-messaging-proxy", log.Fields{"error": err})
			if retries == count {
				return nil, err
			}
			count = +1
			//	Take a nap before retrying
			time.Sleep(2 * time.Second)
		} else {
			break
		}
	}

	probe.UpdateStatusFromContext(ctx, "container-proxy", probe.ServiceStatusRunning)
	log.Info("common-messaging-proxy-created")
	return kip, nil
}

func (a *adapter) startSimulatedONU(ctx context.Context, kip kafka.InterContainerProxy, cp *com.CoreProxy) (*ac.SimulatedONU, error) {
	log.Info("starting-simulated-onu")
	var err error
	sOLT := ac.NewSimulatedONU(ctx, a.kip, cp)

	if err = sOLT.Start(ctx); err != nil {
		log.Fatalw("error-starting-messaging-proxy", log.Fields{"error": err})
		return nil, err
	}

	log.Info("simulated-olt-started")
	return sOLT, nil
}

func (a *adapter) setupRequestHandler(ctx context.Context, coreInstanceId string, iadapter adapters.IAdapter, coreProxy *com.CoreProxy) error {
	log.Info("setting-request-handler")
	requestProxy := com.NewRequestHandlerProxy(coreInstanceId, iadapter, coreProxy)
	if err := a.kip.SubscribeWithRequestHandlerInterface(kafka.Topic{Name: a.config.Topic}, requestProxy); err != nil {
		log.Errorw("adaptercore-request-handler-setup-failed", log.Fields{"error": err})
		return err

	}
	probe.UpdateStatusFromContext(ctx, "core-request-handler", probe.ServiceStatusRunning)
	log.Info("request-handler-setup-done")
	return nil
}
func (a *adapter) registerWithCore(ctx context.Context, retries int) error {
	log.Info("registering-with-core")
	adapterDescription := &voltha.Adapter{
		Id:      "simulated_onu_1",
		Vendor:  "Open Networking Foundation",
		Version: version.VersionInfo.Version,
		Type: "simulated_onu",
		// TODO add parameters to deploy multiple replicas
		CurrentReplica: 1,
		TotalReplicas: 1,
		Endpoint: "simulated_onu",
	}
	types := []*voltha.DeviceType{{Id: "simulated_onu", Adapter: "simulated_onu"}}
	deviceTypes := &voltha.DeviceTypes{Items: types}
	count := 0
	for {
		if err := a.coreProxy.RegisterAdapter(nil, adapterDescription, deviceTypes); err != nil {
			log.Warnw("registering-with-core-failed", log.Fields{"error": err})
			if retries == count {
				return err
			}
			count += 1
			//	Take a nap before retrying
			time.Sleep(2 * time.Second)
		} else {
			break
		}
	}
	probe.UpdateStatusFromContext(ctx, "register-with-core", probe.ServiceStatusRunning)
	log.Info("registered-with-core")
	return nil
}

func waitForExit() int {
	signalChannel := make(chan os.Signal, 1)
	signal.Notify(signalChannel,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)

	exitChannel := make(chan int)

	go func() {
		s := <-signalChannel
		switch s {
		case syscall.SIGHUP,
			syscall.SIGINT,
			syscall.SIGTERM,
			syscall.SIGQUIT:
			log.Infow("closing-signal-received", log.Fields{"signal": s})
			exitChannel <- 0
		default:
			log.Infow("unexpected-signal-received", log.Fields{"signal": s})
			exitChannel <- 1
		}
	}()

	code := <-exitChannel
	return code
}

func printBanner() {
	fmt.Println("	 _                 _       _           _								")
	fmt.Println(" ___(_)_ __ ___  _   _| | __ _| |_ ___  __| |    ___  _ __  _   _			")
	fmt.Println("/ __| | '_ ` _ \\| | | | |/ _` | __/ _ \\/ _` |   / _ \\| '_ \\| | | |		")
	fmt.Println("\\__ \\ | | | | | | |_| | | (_| | ||  __/ (_| |  | (_) | | | | |_| |		")
	fmt.Println("|___/_|_| |_| |_|\\__,_|_|\\__,_|\\__\\___|\\__,_|___\\___/|_| |_|\\__,_|	")
	fmt.Println("                                           |_____|							")
	fmt.Println("																			")
}

func main() {
	start := time.Now()

	cf := config.NewAdapterFlags()
	cf.ParseCommandArguments()

	if cf.PrintVersion {
		fmt.Println(version.VersionInfo.String(""))
		return
	}

	//// Setup logging
	logLevel, err := log.StringToLogLevel(cf.LogLevel)
	if err != nil {
		log.Fatalf("Cannot setup logging, %s", err)
	}

	//Setup default logger - applies for packages that do not have specific logger set
	if _, err := log.SetDefaultLogger(log.JSON, logLevel, log.Fields{"instanceId": cf.InstanceID}); err != nil {
		log.With(log.Fields{"error": err}).Fatal("Cannot setup logging")
	}

	// Update all loggers (provisionned via init) with a common field
	if err := log.UpdateAllLoggers(log.Fields{"instanceId": cf.InstanceID}); err != nil {
		log.With(log.Fields{"error": err}).Fatal("Cannot setup logging")
	}

	log.SetPackageLogLevel("github.com/opencord/voltha-lib-go/v3/pkg/adapters/common", log.DebugLevel)

	defer log.CleanUp()

	// Print banner if specified
	if cf.Banner {
		printBanner()
	}

	log.Infow("config", log.Fields{"config": *cf})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ad := newAdapter(cf)

	p := &probe.Probe{}
	go p.ListenAndServe(fmt.Sprintf("%s:%d", ad.config.ProbeHost, ad.config.ProbePort))
	probeCtx := context.WithValue(ctx, probe.ProbeContextKey, p)
	go ad.start(probeCtx)

	code := waitForExit()
	log.Infow("received-a-closing-signal", log.Fields{"code": code})

	// Cleanup before leaving
	ad.stop(ctx)

	elapsed := time.Since(start)
	log.Infow("runtime", log.Fields{"instanceId": ad.config.InstanceID, "time": elapsed / time.Second})
}
