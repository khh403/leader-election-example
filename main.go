/*
MIT License

Copyright (c) 2023 khh403

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/server/healthz"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog"
)

func buildConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, err
		}
		return cfg, nil
	}

	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return cfg, nil
}

func main() {
	klog.InitFlags(nil)

	var port int
	var kubeconfig string
	var leaseLockName string
	var leaseLockNamespace string
	var id string

	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	// 选线 id, 持有者唯一标识
	flag.StringVar(&id, "id", "", "the holder identity name")
	// etcd 租赁锁资源的名字
	flag.StringVar(&leaseLockName, "lease-lock-name", "", "the lease lock resource name")
	// etcd 租赁锁资源的名字空间
	flag.StringVar(&leaseLockNamespace, "lease-lock-namespace", "",
		"the lease lock resource namespace")
	// 选项 port, 监听端口号
	flag.IntVar(&port, "port", 8080, "port to listen")
	flag.Parse()

	if id == "" {
		klog.Fatal("unable to get id (missing id flag).")
	}
	if leaseLockName == "" {
		klog.Fatal("unable to get lease lock resource name (missing lease-lock-name flag).")
	}
	if leaseLockNamespace == "" {
		klog.Fatal("unable to get lease lock resource namespace (missing lease-lock-namespace flag).")
	}

	// leader election uses the Kubernetes API by writing to a lock object, which can be
	// a LeaseLock object (preferred), a ConfigMap, or an Endpoints (deprecated) object.
	// Conflicting writes are detected and each client handles those actions independently.
	config, err := buildConfig(kubeconfig)
	if err != nil {
		klog.Fatal(err)
	}
	client := clientset.NewForConfigOrDie(config)

	// use a global context in order to tell the leader election code when we want to tear down
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// listen for interrupts or the Linux SIGTERM signal and cancel our context, which
	// the leader election code will observe and step down
	stoppedCh := make(chan os.Signal, 1)
	defer close(stoppedCh)
	signal.Notify(stoppedCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stoppedCh
		klog.Info("Received termination, signaling shutdown")
		cancel()
	}()

	// setup any healthz checks we will want to use.
	var checks []healthz.HealthChecker
	var electionChecker *leaderelection.HealthzAdaptor
	electionChecker = leaderelection.NewLeaderHealthzAdaptor(time.Second * 20)
	checks = append(checks, electionChecker)

	mux := http.NewServeMux()
	healthz.InstallPathHandler(mux, "/healthz", checks...)

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	go func() {
		klog.Infof("Start listening to %d", port)

		if err := server.ListenAndServe(); err != nil {
			klog.Fatalf("Error starting server: %v", err)
		}
	}()

	// complete your controller loop here
	run := func(ctx context.Context) {
		klog.Info("Controller loop...")

		select {}
	}

	klog.Infof("Run with leader election")

	// we use the Lease lock type since edits to Leases are less common
	// and fewer objects in the cluster watch "all Leases".
	// 这里使用租约锁类型是因为对租约的修改不太常见，而且集群中很少有对象会监视“所有租约”。
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      leaseLockName,
			Namespace: leaseLockNamespace,
		},
		Client: client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: id,
		},
	}

	// start the leader election code loop
	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock: lock,
		// IMPORTANT: you MUST ensure that any code you have that
		// is protected by the lease must terminate **before**
		// you call cancel. Otherwise, you could have a background
		// loop still running and another process could
		// get elected before your background loop finished, violating
		// the stated goal of the lease.
		ReleaseOnCancel: true,
		LeaseDuration:   60 * time.Second,
		RenewDeadline:   15 * time.Second,
		RetryPeriod:     5 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				// we're notified when we start - this is where you would
				// usually put your code
				run(ctx)
			},
			OnStoppedLeading: func() {
				// we can do cleanup here
				klog.Infof("lost: %s", id)
				os.Exit(0)
			},
			OnNewLeader: func(identity string) {
				// we're notified when new leader elected
				if identity == id {
					// I just got the lock
					return
				}
				klog.Infof("new leader elected: %s", identity)
			},
		},
		WatchDog: electionChecker,
	})
}
