/*
 *
 * Copyright 2020 gRPC authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package clusterimpl

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"google.golang.org/grpc/balancer"
	"google.golang.org/grpc/balancer/roundrobin"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/internal"
	internalserviceconfig "google.golang.org/grpc/internal/serviceconfig"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/xds/internal/client"
	"google.golang.org/grpc/xds/internal/client/load"
	"google.golang.org/grpc/xds/internal/testutils"
	"google.golang.org/grpc/xds/internal/testutils/fakeclient"
)

const (
	defaultTestTimeout = 1 * time.Second
	testClusterName    = "test-cluster"
	testServiceName    = "test-eds-service"
	testLRSServerName  = "test-lrs-name"
)

var (
	testBackendAddrs = []resolver.Address{
		{Addr: "1.1.1.1:1"},
	}

	cmpOpts = cmp.Options{
		cmpopts.EquateEmpty(),
		cmpopts.IgnoreFields(load.Data{}, "ReportInterval"),
	}
)

func subConnFromPicker(p balancer.Picker) func() balancer.SubConn {
	return func() balancer.SubConn {
		scst, _ := p.Pick(balancer.PickInfo{})
		return scst.SubConn
	}
}

func init() {
	newRandomWRR = testutils.NewTestWRR
}

// TestDropByCategory verifies that the balancer correctly drops the picks, and
// that the drops are reported.
func TestDropByCategory(t *testing.T) {
	defer client.ClearCounterForTesting(testClusterName)
	xdsC := fakeclient.NewClient()
	oldNewXDSClient := newXDSClient
	newXDSClient = func() (xdsClientInterface, error) { return xdsC, nil }
	defer func() { newXDSClient = oldNewXDSClient }()

	builder := balancer.Get(Name)
	cc := testutils.NewTestClientConn(t)
	b := builder.Build(cc, balancer.BuildOptions{})
	defer b.Close()

	const (
		dropReason      = "test-dropping-category"
		dropNumerator   = 1
		dropDenominator = 2
	)
	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState: resolver.State{
			Addresses: testBackendAddrs,
		},
		BalancerConfig: &LBConfig{
			Cluster:                 testClusterName,
			EDSServiceName:          testServiceName,
			LoadReportingServerName: newString(testLRSServerName),
			DropCategories: []DropConfig{{
				Category:           dropReason,
				RequestsPerMillion: million * dropNumerator / dropDenominator,
			}},
			ChildPolicy: &internalserviceconfig.BalancerConfig{
				Name: roundrobin.Name,
			},
		},
	}); err != nil {
		t.Fatalf("unexpected error from UpdateClientConnState: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	got, err := xdsC.WaitForReportLoad(ctx)
	if err != nil {
		t.Fatalf("xdsClient.ReportLoad failed with error: %v", err)
	}
	if got.Server != testLRSServerName {
		t.Fatalf("xdsClient.ReportLoad called with {%q}: want {%q}", got.Server, testLRSServerName)
	}

	sc1 := <-cc.NewSubConnCh
	b.UpdateSubConnState(sc1, balancer.SubConnState{ConnectivityState: connectivity.Connecting})
	// This should get the connecting picker.
	p0 := <-cc.NewPickerCh
	for i := 0; i < 10; i++ {
		_, err := p0.Pick(balancer.PickInfo{})
		if err != balancer.ErrNoSubConnAvailable {
			t.Fatalf("picker.Pick, got _,%v, want Err=%v", err, balancer.ErrNoSubConnAvailable)
		}
	}

	b.UpdateSubConnState(sc1, balancer.SubConnState{ConnectivityState: connectivity.Ready})
	// Test pick with one backend.
	p1 := <-cc.NewPickerCh
	const rpcCount = 20
	for i := 0; i < rpcCount; i++ {
		gotSCSt, err := p1.Pick(balancer.PickInfo{})
		// Even RPCs are dropped.
		if i%2 == 0 {
			if err == nil || !strings.Contains(err.Error(), "dropped") {
				t.Fatalf("pick.Pick, got %v, %v, want error RPC dropped", gotSCSt, err)
			}
			continue
		}
		if err != nil || !cmp.Equal(gotSCSt.SubConn, sc1, cmp.AllowUnexported(testutils.TestSubConn{})) {
			t.Fatalf("picker.Pick, got %v, %v, want SubConn=%v", gotSCSt, err, sc1)
		}
		if gotSCSt.Done != nil {
			gotSCSt.Done(balancer.DoneInfo{})
		}
	}

	// Dump load data from the store and compare with expected counts.
	loadStore := xdsC.LoadStore()
	if loadStore == nil {
		t.Fatal("loadStore is nil in xdsClient")
	}
	const dropCount = rpcCount * dropNumerator / dropDenominator
	wantStatsData0 := []*load.Data{{
		Cluster:    testClusterName,
		Service:    testServiceName,
		TotalDrops: dropCount,
		Drops:      map[string]uint64{dropReason: dropCount},
	}}

	gotStatsData0 := loadStore.Stats([]string{testClusterName})
	if diff := cmp.Diff(gotStatsData0, wantStatsData0, cmpOpts); diff != "" {
		t.Fatalf("got unexpected reports, diff (-got, +want): %v", diff)
	}

	// Send an update with new drop configs.
	const (
		dropReason2      = "test-dropping-category-2"
		dropNumerator2   = 1
		dropDenominator2 = 4
	)
	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState: resolver.State{
			Addresses: testBackendAddrs,
		},
		BalancerConfig: &LBConfig{
			Cluster:                 testClusterName,
			EDSServiceName:          testServiceName,
			LoadReportingServerName: newString(testLRSServerName),
			DropCategories: []DropConfig{{
				Category:           dropReason2,
				RequestsPerMillion: million * dropNumerator2 / dropDenominator2,
			}},
			ChildPolicy: &internalserviceconfig.BalancerConfig{
				Name: roundrobin.Name,
			},
		},
	}); err != nil {
		t.Fatalf("unexpected error from UpdateClientConnState: %v", err)
	}

	p2 := <-cc.NewPickerCh
	for i := 0; i < rpcCount; i++ {
		gotSCSt, err := p2.Pick(balancer.PickInfo{})
		// Even RPCs are dropped.
		if i%4 == 0 {
			if err == nil || !strings.Contains(err.Error(), "dropped") {
				t.Fatalf("pick.Pick, got %v, %v, want error RPC dropped", gotSCSt, err)
			}
			continue
		}
		if err != nil || !cmp.Equal(gotSCSt.SubConn, sc1, cmp.AllowUnexported(testutils.TestSubConn{})) {
			t.Fatalf("picker.Pick, got %v, %v, want SubConn=%v", gotSCSt, err, sc1)
		}
		if gotSCSt.Done != nil {
			gotSCSt.Done(balancer.DoneInfo{})
		}
	}

	const dropCount2 = rpcCount * dropNumerator2 / dropDenominator2
	wantStatsData1 := []*load.Data{{
		Cluster:    testClusterName,
		Service:    testServiceName,
		TotalDrops: dropCount2,
		Drops:      map[string]uint64{dropReason2: dropCount2},
	}}

	gotStatsData1 := loadStore.Stats([]string{testClusterName})
	if diff := cmp.Diff(gotStatsData1, wantStatsData1, cmpOpts); diff != "" {
		t.Fatalf("got unexpected reports, diff (-got, +want): %v", diff)
	}
}

// TestDropCircuitBreaking verifies that the balancer correctly drops the picks
// due to circuit breaking, and that the drops are reported.
func TestDropCircuitBreaking(t *testing.T) {
	defer client.ClearCounterForTesting(testClusterName)
	xdsC := fakeclient.NewClient()
	oldNewXDSClient := newXDSClient
	newXDSClient = func() (xdsClientInterface, error) { return xdsC, nil }
	defer func() { newXDSClient = oldNewXDSClient }()

	builder := balancer.Get(Name)
	cc := testutils.NewTestClientConn(t)
	b := builder.Build(cc, balancer.BuildOptions{})
	defer b.Close()

	var maxRequest uint32 = 50
	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState: resolver.State{
			Addresses: testBackendAddrs,
		},
		BalancerConfig: &LBConfig{
			Cluster:                 testClusterName,
			EDSServiceName:          testServiceName,
			LoadReportingServerName: newString(testLRSServerName),
			MaxConcurrentRequests:   &maxRequest,
			ChildPolicy: &internalserviceconfig.BalancerConfig{
				Name: roundrobin.Name,
			},
		},
	}); err != nil {
		t.Fatalf("unexpected error from UpdateClientConnState: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaultTestTimeout)
	defer cancel()

	got, err := xdsC.WaitForReportLoad(ctx)
	if err != nil {
		t.Fatalf("xdsClient.ReportLoad failed with error: %v", err)
	}
	if got.Server != testLRSServerName {
		t.Fatalf("xdsClient.ReportLoad called with {%q}: want {%q}", got.Server, testLRSServerName)
	}

	sc1 := <-cc.NewSubConnCh
	b.UpdateSubConnState(sc1, balancer.SubConnState{ConnectivityState: connectivity.Connecting})
	// This should get the connecting picker.
	p0 := <-cc.NewPickerCh
	for i := 0; i < 10; i++ {
		_, err := p0.Pick(balancer.PickInfo{})
		if err != balancer.ErrNoSubConnAvailable {
			t.Fatalf("picker.Pick, got _,%v, want Err=%v", err, balancer.ErrNoSubConnAvailable)
		}
	}

	b.UpdateSubConnState(sc1, balancer.SubConnState{ConnectivityState: connectivity.Ready})
	// Test pick with one backend.
	dones := []func(){}
	p1 := <-cc.NewPickerCh
	const rpcCount = 100
	for i := 0; i < rpcCount; i++ {
		gotSCSt, err := p1.Pick(balancer.PickInfo{})
		if i < 50 && err != nil {
			t.Errorf("The first 50%% picks should be non-drops, got error %v", err)
		} else if i > 50 && err == nil {
			t.Errorf("The second 50%% picks should be drops, got error <nil>")
		}
		dones = append(dones, func() {
			if gotSCSt.Done != nil {
				gotSCSt.Done(balancer.DoneInfo{})
			}
		})
	}
	for _, done := range dones {
		done()
	}

	dones = []func(){}
	// Pick without drops.
	for i := 0; i < 50; i++ {
		gotSCSt, err := p1.Pick(balancer.PickInfo{})
		if err != nil {
			t.Errorf("The third 50%% picks should be non-drops, got error %v", err)
		}
		dones = append(dones, func() {
			if gotSCSt.Done != nil {
				gotSCSt.Done(balancer.DoneInfo{})
			}
		})
	}
	for _, done := range dones {
		done()
	}

	// Dump load data from the store and compare with expected counts.
	loadStore := xdsC.LoadStore()
	if loadStore == nil {
		t.Fatal("loadStore is nil in xdsClient")
	}

	wantStatsData0 := []*load.Data{{
		Cluster:    testClusterName,
		Service:    testServiceName,
		TotalDrops: uint64(maxRequest),
	}}

	gotStatsData0 := loadStore.Stats([]string{testClusterName})
	if diff := cmp.Diff(gotStatsData0, wantStatsData0, cmpOpts); diff != "" {
		t.Fatalf("got unexpected drop reports, diff (-got, +want): %v", diff)
	}
}

// TestPickerUpdateAfterClose covers the case that cluster_impl wants to update
// picker after it's closed. Because picker updates are sent in the run()
// goroutine.
func TestPickerUpdateAfterClose(t *testing.T) {
	defer client.ClearCounterForTesting(testClusterName)
	xdsC := fakeclient.NewClient()
	oldNewXDSClient := newXDSClient
	newXDSClient = func() (xdsClientInterface, error) { return xdsC, nil }
	defer func() { newXDSClient = oldNewXDSClient }()

	builder := balancer.Get(Name)
	cc := testutils.NewTestClientConn(t)
	b := builder.Build(cc, balancer.BuildOptions{})

	var maxRequest uint32 = 50
	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState: resolver.State{
			Addresses: testBackendAddrs,
		},
		BalancerConfig: &LBConfig{
			Cluster:               testClusterName,
			EDSServiceName:        testServiceName,
			MaxConcurrentRequests: &maxRequest,
			ChildPolicy: &internalserviceconfig.BalancerConfig{
				Name: roundrobin.Name,
			},
		},
	}); err != nil {
		b.Close()
		t.Fatalf("unexpected error from UpdateClientConnState: %v", err)
	}

	// Send SubConn state changes to trigger picker updates. Balancer will
	// closed in a defer.
	sc1 := <-cc.NewSubConnCh
	b.UpdateSubConnState(sc1, balancer.SubConnState{ConnectivityState: connectivity.Connecting})
	// This close will race with the SubConn state update.
	b.Close()

	select {
	case <-cc.NewPickerCh:
		t.Fatalf("unexpected picker update after balancer is closed")
	case <-time.After(time.Millisecond * 10):
	}
}

// TestClusterNameInAddressAttributes covers the case that cluster name is
// attached to the subconn address attributes.
func TestClusterNameInAddressAttributes(t *testing.T) {
	defer client.ClearCounterForTesting(testClusterName)
	xdsC := fakeclient.NewClient()
	oldNewXDSClient := newXDSClient
	newXDSClient = func() (xdsClientInterface, error) { return xdsC, nil }
	defer func() { newXDSClient = oldNewXDSClient }()

	builder := balancer.Get(Name)
	cc := testutils.NewTestClientConn(t)
	b := builder.Build(cc, balancer.BuildOptions{})
	defer b.Close()

	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState: resolver.State{
			Addresses: testBackendAddrs,
		},
		BalancerConfig: &LBConfig{
			Cluster:        testClusterName,
			EDSServiceName: testServiceName,
			ChildPolicy: &internalserviceconfig.BalancerConfig{
				Name: roundrobin.Name,
			},
		},
	}); err != nil {
		t.Fatalf("unexpected error from UpdateClientConnState: %v", err)
	}

	sc1 := <-cc.NewSubConnCh
	b.UpdateSubConnState(sc1, balancer.SubConnState{ConnectivityState: connectivity.Connecting})
	// This should get the connecting picker.
	p0 := <-cc.NewPickerCh
	for i := 0; i < 10; i++ {
		_, err := p0.Pick(balancer.PickInfo{})
		if err != balancer.ErrNoSubConnAvailable {
			t.Fatalf("picker.Pick, got _,%v, want Err=%v", err, balancer.ErrNoSubConnAvailable)
		}
	}

	addrs1 := <-cc.NewSubConnAddrsCh
	if got, want := addrs1[0].Addr, testBackendAddrs[0].Addr; got != want {
		t.Fatalf("sc is created with addr %v, want %v", got, want)
	}
	cn, ok := internal.GetXDSHandshakeClusterName(addrs1[0].Attributes)
	if !ok || cn != testClusterName {
		t.Fatalf("sc is created with addr with cluster name %v, %v, want cluster name %v", cn, ok, testClusterName)
	}

	b.UpdateSubConnState(sc1, balancer.SubConnState{ConnectivityState: connectivity.Ready})
	// Test pick with one backend.
	p1 := <-cc.NewPickerCh
	const rpcCount = 20
	for i := 0; i < rpcCount; i++ {
		gotSCSt, err := p1.Pick(balancer.PickInfo{})
		if err != nil || !cmp.Equal(gotSCSt.SubConn, sc1, cmp.AllowUnexported(testutils.TestSubConn{})) {
			t.Fatalf("picker.Pick, got %v, %v, want SubConn=%v", gotSCSt, err, sc1)
		}
		if gotSCSt.Done != nil {
			gotSCSt.Done(balancer.DoneInfo{})
		}
	}

	const testClusterName2 = "test-cluster-2"
	var addr2 = resolver.Address{Addr: "2.2.2.2"}
	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState: resolver.State{
			Addresses: []resolver.Address{addr2},
		},
		BalancerConfig: &LBConfig{
			Cluster:        testClusterName2,
			EDSServiceName: testServiceName,
			ChildPolicy: &internalserviceconfig.BalancerConfig{
				Name: roundrobin.Name,
			},
		},
	}); err != nil {
		t.Fatalf("unexpected error from UpdateClientConnState: %v", err)
	}

	addrs2 := <-cc.NewSubConnAddrsCh
	if got, want := addrs2[0].Addr, addr2.Addr; got != want {
		t.Fatalf("sc is created with addr %v, want %v", got, want)
	}
	// New addresses should have the new cluster name.
	cn2, ok := internal.GetXDSHandshakeClusterName(addrs2[0].Attributes)
	if !ok || cn2 != testClusterName2 {
		t.Fatalf("sc is created with addr with cluster name %v, %v, want cluster name %v", cn2, ok, testClusterName2)
	}
}

// TestReResolution verifies that when a SubConn turns transient failure,
// re-resolution is triggered.
func TestReResolution(t *testing.T) {
	defer client.ClearCounterForTesting(testClusterName)
	xdsC := fakeclient.NewClient()
	oldNewXDSClient := newXDSClient
	newXDSClient = func() (xdsClientInterface, error) { return xdsC, nil }
	defer func() { newXDSClient = oldNewXDSClient }()

	builder := balancer.Get(Name)
	cc := testutils.NewTestClientConn(t)
	b := builder.Build(cc, balancer.BuildOptions{})
	defer b.Close()

	if err := b.UpdateClientConnState(balancer.ClientConnState{
		ResolverState: resolver.State{
			Addresses: testBackendAddrs,
		},
		BalancerConfig: &LBConfig{
			Cluster:        testClusterName,
			EDSServiceName: testServiceName,
			ChildPolicy: &internalserviceconfig.BalancerConfig{
				Name: roundrobin.Name,
			},
		},
	}); err != nil {
		t.Fatalf("unexpected error from UpdateClientConnState: %v", err)
	}

	sc1 := <-cc.NewSubConnCh
	b.UpdateSubConnState(sc1, balancer.SubConnState{ConnectivityState: connectivity.Connecting})
	// This should get the connecting picker.
	p0 := <-cc.NewPickerCh
	for i := 0; i < 10; i++ {
		_, err := p0.Pick(balancer.PickInfo{})
		if err != balancer.ErrNoSubConnAvailable {
			t.Fatalf("picker.Pick, got _,%v, want Err=%v", err, balancer.ErrNoSubConnAvailable)
		}
	}

	b.UpdateSubConnState(sc1, balancer.SubConnState{ConnectivityState: connectivity.TransientFailure})
	// This should get the transient failure picker.
	p1 := <-cc.NewPickerCh
	for i := 0; i < 10; i++ {
		_, err := p1.Pick(balancer.PickInfo{})
		if err == nil {
			t.Fatalf("picker.Pick, got _,%v, want not nil", err)
		}
	}

	// The transient failure should trigger a re-resolution.
	select {
	case <-cc.ResolveNowCh:
	case <-time.After(defaultTestTimeout):
		t.Fatalf("timeout waiting for ResolveNow()")
	}

	b.UpdateSubConnState(sc1, balancer.SubConnState{ConnectivityState: connectivity.Ready})
	// Test pick with one backend.
	p2 := <-cc.NewPickerCh
	want := []balancer.SubConn{sc1}
	if err := testutils.IsRoundRobin(want, subConnFromPicker(p2)); err != nil {
		t.Fatalf("want %v, got %v", want, err)
	}

	b.UpdateSubConnState(sc1, balancer.SubConnState{ConnectivityState: connectivity.TransientFailure})
	// This should get the transient failure picker.
	p3 := <-cc.NewPickerCh
	for i := 0; i < 10; i++ {
		_, err := p3.Pick(balancer.PickInfo{})
		if err == nil {
			t.Fatalf("picker.Pick, got _,%v, want not nil", err)
		}
	}

	// The transient failure should trigger a re-resolution.
	select {
	case <-cc.ResolveNowCh:
	case <-time.After(defaultTestTimeout):
		t.Fatalf("timeout waiting for ResolveNow()")
	}
}
