/*
Copyright 2026.

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

package controller

// Reconcile-churn reproduction.
//
// Every other test in this package calls Reconcile() directly, so the watch
// feedback path — status write → resourceVersion bump → watch event → another
// controller reconciles → writes its own status — simply does not exist there.
// That is why the suite is blind to the hot loops these tests reproduce.
//
// These specs start a real ctrl.NewManager against the envtest API server so
// the informers, workqueues and watch fan-out are all live, create one
// LangfuseInstance, then sit still and measure how much the operator churns on
// its own with zero external input. At steady state a healthy operator should
// be silent.
//
// Churn is measured two independent ways so the numbers do not depend on
// controller-runtime internals being trustworthy:
//
//  1. controller_runtime_reconcile_total, per controller, from the global
//     controller-runtime metrics registry, as a delta over a fixed window.
//  2. Implementation-agnostic: poll the CR's resourceVersion and count distinct
//     values, plus the sequence of status.phase values observed (which is what
//     exposes the Pending↔Degraded alternation) and the Deployment
//     metadata.generation, which only advances when something actually writes
//     the Deployment spec.
//
// Probe targets are deliberately connection-refused (127.0.0.1:1) rather than
// black-holed: a refused TCP connect fails in microseconds, whereas a dropped
// SYN blocks for the full 3s probeTimeout and would make each health-monitor
// pass dominate the observation window instead of the loop itself.

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/PalenaAI/langfuse-operator/api/v1alpha1"
	"github.com/PalenaAI/langfuse-operator/internal/langfuse"
	"github.com/PalenaAI/langfuse-operator/internal/resources"
)

const (
	// churnSettle lets the first reconcile burst (create Deployments, Services,
	// NetworkPolicies, ...) finish, so the observation window measures steady
	// state rather than initial convergence.
	churnSettle = 5 * time.Second
	// churnWindow is the observation window. Rates are reported per second so
	// the exact length is not load-bearing.
	churnWindow = 15 * time.Second
	// churnPollInterval bounds the resourceVersion sampling resolution: bumps
	// faster than 2Hz are undercounted, so distinctRVs is a floor, never a
	// ceiling.
	churnPollInterval = 500 * time.Millisecond

	// churnBudget is a deliberately generous steady-state allowance. A correct
	// operator does zero reconciles here; the health monitor's own 30s requeue
	// permits at most one per controller in a 15s window.
	churnBudget = 10
)

// ─── observation ────────────────────────────────────────────────────────────

// churnOpts configures one observation window.
type churnOpts struct {
	// window overrides churnWindow when non-zero.
	window time.Duration
	// deployments are tracked for generation and spec.replicas churn.
	deployments []string
	// poke, when set, writes an annotation to the CR every pokeInterval. It
	// stands in for whatever else touches the object in a real cluster (another
	// controller, a user, a GitOps sync) and lets a spec measure how many
	// self-inflicted writes the operator adds per external event.
	poke         bool
	pokeInterval time.Duration
}

func (o churnOpts) observationWindow() time.Duration {
	if o.window != 0 {
		return o.window
	}
	return churnWindow
}

// churnReport is everything measured over one observation window.
type churnReport struct {
	window      time.Duration
	pokes       int
	reconciles  map[string]float64 // controller name → reconciles in window
	rvSamples   int
	distinctRVs int                // floor: sampled at churnPollInterval
	rvBumps     int                // exact: MODIFIED events from an API-server watch
	writeCalls  float64            // PUT/PATCH API calls issued, incl. ones the server absorbs
	phases      []string           // consecutive duplicates collapsed
	generations map[string]int64   // deployment name → generation bumps in window
	replicas    map[string][]int32 // deployment name → distinct replica values seen, in order
}

// churnWatchClient is a lazily-built watch-capable client. suite_test.go's
// k8sClient cannot watch, and building one per spec would leak connections.
var churnWatchClient client.WithWatch

func watchClient() client.WithWatch {
	GinkgoHelper()
	if churnWatchClient == nil {
		var err error
		churnWatchClient, err = client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
	}
	return churnWatchClient
}

// reconcileTotals reads controller_runtime_reconcile_total out of the global
// controller-runtime registry, summed over the `result` label, keyed by
// controller name. Absolute counters are process-global and accumulate across
// specs, so callers must always diff two snapshots.
func reconcileTotals() map[string]float64 {
	totals := map[string]float64{}
	families, err := metrics.Registry.Gather()
	Expect(err).NotTo(HaveOccurred())
	for _, family := range families {
		if family.GetName() != "controller_runtime_reconcile_total" {
			continue
		}
		for _, m := range family.GetMetric() {
			var name string
			for _, l := range m.GetLabel() {
				if l.GetName() == "controller" {
					name = l.GetValue()
				}
			}
			totals[name] += m.GetCounter().GetValue()
		}
	}
	return totals
}

// putRequests reads rest_client_requests_total (registered into the same
// registry by controller-runtime's client-go adapter) for write verbs. It
// counts API calls the operator actually issued, which is what distinguishes
// "the DeepEqual gate never fires" from "it fires and the write is a no-op the
// API server absorbs".
func putRequests() float64 {
	var total float64
	families, err := metrics.Registry.Gather()
	Expect(err).NotTo(HaveOccurred())
	for _, family := range families {
		if family.GetName() != "rest_client_requests_total" {
			continue
		}
		for _, m := range family.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "method" && (l.GetValue() == "PUT" || l.GetValue() == "PATCH") {
					total += m.GetCounter().GetValue()
				}
			}
		}
	}
	return total
}

// observeChurn watches a settled instance and reports what moved. Unless
// opts.poke is set it takes no action of its own, so every write it counts is
// the operator talking to itself.
func observeChurn(key types.NamespacedName, opts churnOpts) churnReport {
	GinkgoHelper()

	window := opts.observationWindow()
	deployments := opts.deployments

	By(fmt.Sprintf("settling for %s", churnSettle))
	time.Sleep(churnSettle)

	before := reconcileTotals()
	putsBefore := putRequests()
	genBefore := map[string]int64{}
	for _, name := range deployments {
		genBefore[name] = deploymentGeneration(key.Namespace, name)
	}

	report := churnReport{
		window:      window,
		generations: map[string]int64{},
		replicas:    map[string][]int32{},
	}

	// Exact RV bump count, straight from the API server's own change stream —
	// no controller-runtime internals involved. The 500ms sampler below
	// saturates once the loop exceeds 2Hz, so this is what makes the real rate
	// visible.
	watchCtx, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	watcher, err := watchClient().Watch(watchCtx, &v1alpha1.LangfuseInstanceList{},
		client.InNamespace(key.Namespace), client.MatchingFields{"metadata.name": key.Name})
	Expect(err).NotTo(HaveOccurred())
	defer watcher.Stop()

	bumps := make(chan int, 1)
	go func() {
		defer GinkgoRecover()
		count := 0
		for event := range watcher.ResultChan() {
			if event.Type == watch.Modified {
				count++
			}
		}
		bumps <- count
	}()

	By(fmt.Sprintf("observing for %s", window))
	seenRV := map[string]bool{}
	deadline := time.Now().Add(window)
	nextPoke := time.Now()
	for time.Now().Before(deadline) {
		if opts.poke && !time.Now().Before(nextPoke) {
			pokeInstance(key, report.pokes)
			report.pokes++
			nextPoke = time.Now().Add(opts.pokeInterval)
		}
		instance := &v1alpha1.LangfuseInstance{}
		if err := k8sClient.Get(context.Background(), key, instance); err == nil {
			report.rvSamples++
			if !seenRV[instance.ResourceVersion] {
				seenRV[instance.ResourceVersion] = true
			}
			phase := instance.Status.Phase
			if phase == "" {
				phase = "<empty>"
			}
			if len(report.phases) == 0 || report.phases[len(report.phases)-1] != phase {
				report.phases = append(report.phases, phase)
			}
		}
		for _, name := range deployments {
			deploy := &appsv1.Deployment{}
			if err := k8sClient.Get(context.Background(),
				types.NamespacedName{Name: name, Namespace: key.Namespace}, deploy); err != nil {
				continue
			}
			want := ptr.Deref(deploy.Spec.Replicas, -1)
			seen := report.replicas[name]
			if len(seen) == 0 || seen[len(seen)-1] != want {
				report.replicas[name] = append(seen, want)
			}
		}
		time.Sleep(churnPollInterval)
	}
	report.distinctRVs = len(seenRV)

	watcher.Stop()
	Eventually(bumps, "10s").Should(Receive(&report.rvBumps))

	after := reconcileTotals()
	report.writeCalls = putRequests() - putsBefore
	report.reconciles = map[string]float64{}
	for name, count := range after {
		if delta := count - before[name]; delta > 0 {
			report.reconciles[name] = delta
		}
	}
	for _, name := range deployments {
		report.generations[name] = deploymentGeneration(key.Namespace, name) - genBefore[name]
	}

	AddReportEntry("churn", report.String())
	GinkgoWriter.Println(report.String())
	return report
}

// pokeInstance writes one annotation to the CR, standing in for any external
// touch a real cluster produces. It changes metadata only, so generation is
// untouched and the write is unambiguously ours.
func pokeInstance(key types.NamespacedName, n int) {
	GinkgoHelper()

	instance := &v1alpha1.LangfuseInstance{}
	Expect(k8sClient.Get(context.Background(), key, instance)).To(Succeed())
	if instance.Annotations == nil {
		instance.Annotations = map[string]string{}
	}
	instance.Annotations["churn.test/poke"] = fmt.Sprintf("%d", n)
	Expect(k8sClient.Update(context.Background(), instance)).To(Succeed())
}

func deploymentGeneration(namespace, name string) int64 {
	deploy := &appsv1.Deployment{}
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: name, Namespace: namespace}, deploy); err != nil {
		return 0
	}
	return deploy.Generation
}

// totalReconciles is the sum across every controller that ran in the window.
func (r churnReport) totalReconciles() float64 {
	var total float64
	for _, count := range r.reconciles {
		total += count
	}
	return total
}

// budgetViolations lists every steady-state invariant the window broke.
// Collecting them instead of asserting one at a time means a single run shows
// the full picture rather than stopping at the first failure.
func (r churnReport) budgetViolations() []string {
	var v []string
	if total := r.totalReconciles(); total >= churnBudget {
		v = append(v, fmt.Sprintf("%.0f reconciles in %s (%.2f/s) — budget is <%d",
			total, r.window, total/r.window.Seconds(), churnBudget))
	}
	if selfWrites := r.rvBumps - r.pokes; selfWrites >= churnBudget {
		v = append(v, fmt.Sprintf("%d self-inflicted CR resourceVersion bumps in %s (%.2f/s) — budget is <%d",
			selfWrites, r.window, float64(selfWrites)/r.window.Seconds(), churnBudget))
	}
	if len(r.phases) > 1 {
		v = append(v, fmt.Sprintf("status.phase alternated %d times: %s",
			len(r.phases)-1, strings.Join(r.phases, "→")))
	}
	for _, name := range sortedKeys(r.generations) {
		if r.generations[name] > 0 {
			v = append(v, fmt.Sprintf("deployment %s spec rewritten %d times in %s",
				name, r.generations[name], r.window))
		}
	}
	return v
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (r churnReport) String() string {
	var b strings.Builder
	secs := r.window.Seconds()
	fmt.Fprintf(&b, "\n── reconcile churn over %s ──\n", r.window)

	names := make([]string, 0, len(r.reconciles))
	for name := range r.reconciles {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		fmt.Fprintf(&b, "  reconciles:        none\n")
	}
	for _, name := range names {
		fmt.Fprintf(&b, "  reconciles[%-18s] %8.0f  (%6.2f/s)\n",
			name+"]", r.reconciles[name], r.reconciles[name]/secs)
	}
	fmt.Fprintf(&b, "  reconciles[%-18s] %8.0f  (%6.2f/s)\n",
		"TOTAL]", r.totalReconciles(), r.totalReconciles()/secs)
	fmt.Fprintf(&b, "  write API calls (PUT+PATCH): %8.0f  (%6.2f/s)\n",
		r.writeCalls, r.writeCalls/secs)
	fmt.Fprintf(&b, "  CR resourceVersion bumps (watch, exact): %d  (%.2f/s)\n",
		r.rvBumps, float64(r.rvBumps)/secs)
	if r.pokes > 0 {
		fmt.Fprintf(&b, "    of which external test pokes: %d → %.1f operator writes per external event\n",
			r.pokes, float64(r.rvBumps-r.pokes)/float64(r.pokes))
	}
	fmt.Fprintf(&b, "  distinct CR resourceVersions (sampled @%s): %d of %d samples",
		churnPollInterval, r.distinctRVs, r.rvSamples)
	if r.distinctRVs == r.rvSamples && r.rvSamples > 0 {
		b.WriteString("  ← saturated, sampler cannot keep up")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "  status.phase sequence: %s\n", strings.Join(r.phases, " → "))

	deployNames := make([]string, 0, len(r.generations))
	for name := range r.generations {
		deployNames = append(deployNames, name)
	}
	sort.Strings(deployNames)
	for _, name := range deployNames {
		fmt.Fprintf(&b, "  deployment %s: generation +%d  (%.2f spec writes/s)",
			name, r.generations[name], float64(r.generations[name])/secs)
		if values := r.replicas[name]; len(values) > 1 {
			fmt.Fprintf(&b, ", spec.replicas %v", values)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// ─── spec diffing (H5) ──────────────────────────────────────────────────────

// maxSpecDiffs bounds the reported diff so a wholly-different spec cannot
// produce unreadable output.
const maxSpecDiffs = 40

// specDifferences lists the field paths on which equality.Semantic.DeepEqual —
// the exact comparison reconcileDeployment gates its Update on — sees the live
// (API-server-defaulted) spec and a freshly-built desired spec as different.
// An empty result means the gate correctly stays shut when nothing changed.
func specDifferences(live, desired appsv1.DeploymentSpec) []string {
	if equality.Semantic.DeepEqual(live, desired) {
		return nil
	}
	var diffs []string
	walkSpecDiff("spec", reflect.ValueOf(live), reflect.ValueOf(desired), &diffs)
	return diffs
}

func walkSpecDiff(path string, live, desired reflect.Value, diffs *[]string) {
	if len(*diffs) >= maxSpecDiffs {
		return
	}
	if live.IsValid() && desired.IsValid() && equality.Semantic.DeepEqual(live.Interface(), desired.Interface()) {
		return
	}

	switch live.Kind() {
	case reflect.Pointer, reflect.Interface:
		switch {
		case live.IsNil() && desired.IsNil():
			return
		case desired.IsNil():
			recordSpecDiff(path, live, desired, diffs, "builder leaves it unset")
		case live.IsNil():
			recordSpecDiff(path, live, desired, diffs, "")
		default:
			walkSpecDiff(path, live.Elem(), desired.Elem(), diffs)
		}
	case reflect.Struct:
		if hasUnexportedFields(live.Type()) {
			recordSpecDiff(path, live, desired, diffs, "")
			return
		}
		for i := 0; i < live.NumField(); i++ {
			walkSpecDiff(path+"."+fieldJSONName(live.Type().Field(i)),
				live.Field(i), desired.Field(i), diffs)
		}
	case reflect.Slice, reflect.Array:
		if live.Len() != desired.Len() {
			recordSpecDiff(path, live, desired, diffs, "")
			return
		}
		for i := 0; i < live.Len(); i++ {
			walkSpecDiff(fmt.Sprintf("%s[%d]", path, i), live.Index(i), desired.Index(i), diffs)
		}
	default:
		note := ""
		if desired.IsZero() && !live.IsZero() {
			note = "builder leaves it unset"
		}
		recordSpecDiff(path, live, desired, diffs, note)
	}
}

func recordSpecDiff(path string, live, desired reflect.Value, diffs *[]string, note string) {
	entry := fmt.Sprintf("%s: live=%s desired=%s", path, briefValue(live), briefValue(desired))
	if note != "" {
		entry += "  ← " + note
	}
	*diffs = append(*diffs, entry)
}

func hasUnexportedFields(t reflect.Type) bool {
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).PkgPath != "" {
			return true
		}
	}
	return false
}

func fieldJSONName(f reflect.StructField) string {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name
	}
	if name := strings.Split(tag, ",")[0]; name != "" && name != "-" {
		return name
	}
	return f.Name
}

func briefValue(v reflect.Value) string {
	if !v.IsValid() {
		return "<invalid>"
	}
	if v.Kind() == reflect.Pointer && v.IsNil() {
		return "nil"
	}
	if v.Kind() == reflect.Pointer {
		return briefValue(v.Elem())
	}
	s := fmt.Sprintf("%v", v.Interface())
	if s == "" {
		return `""`
	}
	if len(s) > 60 {
		return s[:57] + "..."
	}
	return s
}

// ─── fixtures ───────────────────────────────────────────────────────────────

const churnEndpointSecret = "churn-endpoints"

// newChurnNamespace creates a dedicated namespace plus the endpoint Secret the
// instance points at, and registers cleanup.
func newChurnNamespace(name string) string {
	GinkgoHelper()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	Expect(k8sClient.Create(context.Background(), ns)).To(Succeed())

	// Connection-refused targets: the probes fail in microseconds instead of
	// blocking for the full 3s probeTimeout.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: churnEndpointSecret, Namespace: name},
		StringData: map[string]string{
			"database_url":   "postgres://langfuse:pw@127.0.0.1:1/langfuse",
			"clickhouse_url": "http://127.0.0.1:1",
			"redis_host":     "127.0.0.1",
			"redis_port":     "1",
		},
	}
	Expect(k8sClient.Create(context.Background(), secret)).To(Succeed())

	DeferCleanup(func() {
		// envtest has no namespace controller, so a namespace Delete never
		// completes. Drop the CR explicitly; the rest dies with the API server.
		instances := &v1alpha1.LangfuseInstanceList{}
		if err := k8sClient.List(context.Background(), instances, client.InNamespace(name)); err == nil {
			for i := range instances.Items {
				_ = k8sClient.Delete(context.Background(), &instances.Items[i])
			}
		}
	})
	return name
}

// newChurnInstance builds a minimal-but-valid LangfuseInstance whose three
// datastores all resolve to a closed port.
func newChurnInstance(namespace, name string) *v1alpha1.LangfuseInstance {
	secretRef := func(keys map[string]string) v1alpha1.SecretKeysRef {
		return v1alpha1.SecretKeysRef{Name: churnEndpointSecret, Keys: keys}
	}
	return &v1alpha1.LangfuseInstance{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.LangfuseInstanceSpec{
			Image: v1alpha1.ImageSpec{Repository: "langfuse/langfuse", Tag: "3.126.0"},
			Auth:  v1alpha1.AuthSpec{NextAuthUrl: "http://langfuse.churn.invalid"},
			Database: &v1alpha1.DatabaseSpec{
				External: &v1alpha1.ExternalDatabaseSpec{
					SecretRef: secretRef(map[string]string{"url": "database_url"}),
				},
			},
			ClickHouse: &v1alpha1.ClickHouseSpec{
				External: &v1alpha1.ExternalClickHouseSpec{
					SecretRef: secretRef(map[string]string{"url": "clickhouse_url"}),
				},
			},
			Redis: &v1alpha1.RedisSpec{
				External: &v1alpha1.ExternalRedisSpec{
					SecretRef: secretRef(map[string]string{"host": "redis_host", "port": "redis_port"}),
				},
			},
		},
	}
}

// startChurnManager brings up a real manager with only the controllers under
// test, so each spec isolates one loop.
//
// SkipNameValidation is required because controller-runtime's uniqueness check
// is process-global (pkg/controller/name.go) and each SetupWithManager here
// hardcodes its Named(); without it the second spec to register a given
// controller fails. Metrics BindAddress is "0" so concurrent managers do not
// clash on :8080.
func startChurnManager(register ...func(ctrl.Manager) error) {
	GinkgoHelper()

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:  scheme.Scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
		Controller: crconfig.Controller{
			SkipNameValidation: ptr.To(true),
		},
	})
	Expect(err).NotTo(HaveOccurred())

	for _, setup := range register {
		Expect(setup(mgr)).To(Succeed())
	}

	mgrCtx, stopMgr := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer GinkgoRecover()
		defer close(stopped)
		Expect(mgr.Start(mgrCtx)).To(Succeed())
	}()

	DeferCleanup(func() {
		stopMgr()
		Eventually(stopped, "30s").Should(BeClosed())
	})

	Expect(mgr.GetCache().WaitForCacheSync(mgrCtx)).To(BeTrue())
}

// ─── H2: phase ping-pong between the instance controller and health monitor ──

var _ = Describe("Reconcile churn", func() {
	Describe("H2: instance controller + health monitor", func() {
		const (
			namespace = "churn-h2"
			name      = "churn"
		)

		It("stays quiet once converged", func() {
			ns := newChurnNamespace(namespace)

			startChurnManager(
				func(mgr ctrl.Manager) error {
					return (&LangfuseInstanceReconciler{
						Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
					}).SetupWithManager(mgr)
				},
				func(mgr ctrl.Manager) error {
					return (&HealthMonitorReconciler{
						Client:   mgr.GetClient(),
						Scheme:   mgr.GetScheme(),
						Recorder: mgr.GetEventRecorderFor("health-monitor"),
					}).SetupWithManager(mgr)
				},
			)

			instance := newChurnInstance(ns, name)
			Expect(k8sClient.Create(context.Background(), instance)).To(Succeed())

			key := types.NamespacedName{Name: name, Namespace: ns}
			Eventually(func() error {
				return k8sClient.Get(context.Background(),
					types.NamespacedName{Name: resources.WebName(instance), Namespace: ns}, &appsv1.Deployment{})
			}, "30s").Should(Succeed(), "web deployment should be created")

			report := observeChurn(key, churnOpts{
				deployments: []string{resources.WebName(instance), resources.WorkerName(instance)},
			})

			// There is no kubelet in envtest, so the Deployments never become
			// ready: updateStatus() writes Pending while determineOverallHealth()
			// writes Degraded, and neither checks whether the value changed.
			// Each write bumps resourceVersion, which wakes the other controller
			// through its unfiltered For() watch, forever.
			Expect(report.budgetViolations()).To(BeEmpty(),
				"the operator should be idle with no external input")
		})
	})

	// ─── H3: SecretController's per-pass wall-clock timestamp ────────────────

	Describe("H3: secret controller alone", func() {
		const (
			namespace = "churn-h3"
			name      = "churn"
		)

		It("adds no status writes of its own", func() {
			ns := newChurnNamespace(namespace)

			startChurnManager(func(mgr ctrl.Manager) error {
				return (&SecretController{
					Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
				}).SetupWithManager(mgr)
			})

			instance := newChurnInstance(ns, name)
			Expect(k8sClient.Create(context.Background(), instance)).To(Succeed())

			key := types.NamespacedName{Name: name, Namespace: ns}
			Eventually(func() error {
				return k8sClient.Get(context.Background(),
					types.NamespacedName{Name: name + generatedSecretSuffix, Namespace: ns}, &corev1.Secret{})
			}, "30s").Should(Succeed(), "generated secret should be created")

			// Part 1 — in isolation the controller goes quiet, contrary to the
			// "guaranteed self-trigger" hypothesis. status.secrets.lastRotationCheck
			// is a metav1.Time, which serializes as RFC3339 with 1s granularity, so
			// the second pass of a burst re-writes the *same* timestamp. The API
			// server suppresses the no-op, no resourceVersion bump is emitted, no
			// watch event fires — and SecretController returns a bare ctrl.Result{}
			// with no RequeueAfter, so nothing wakes it again. Recorded for the
			// record; the defect is in part 2.
			By("measuring the undisturbed instance")
			idle := observeChurn(key, churnOpts{window: 8 * time.Second})
			AddReportEntry("h3-idle", idle.String())

			// Part 2 — the real cost. Every external touch spaced more than a
			// second from the last pass makes the timestamp a *different* value, so
			// the unconditional Status().Update lands for real and costs an extra
			// resourceVersion bump. Via H1 that bump fans out to all seven
			// controllers, so one external event becomes seven extra reconciles.
			By("measuring one external touch every 1.5s")
			driven := observeChurn(key, churnOpts{poke: true, pokeInterval: 1500 * time.Millisecond})

			selfWrites := driven.rvBumps - driven.pokes
			Expect(selfWrites).To(BeNumerically("<=", 0),
				"secret controller added %d status writes on top of %d external events "+
					"(%.1f per event); it should add none",
				selfWrites, driven.pokes, float64(selfWrites)/float64(driven.pokes))
		})
	})

	// ─── H5: builders vs. API-server defaulting ──────────────────────────────

	Describe("H5: change detection and field ownership", func() {
		const (
			namespace = "churn-h5"
			name      = "churn"
		)

		It("leaves Deployment fields owned by another manager alone", func() {
			ns := newChurnNamespace(namespace)

			startChurnManager(func(mgr ctrl.Manager) error {
				return (&LangfuseInstanceReconciler{
					Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
				}).SetupWithManager(mgr)
			})

			instance := newChurnInstance(ns, name)
			Expect(k8sClient.Create(context.Background(), instance)).To(Succeed())

			webName := resources.WebName(instance)
			webKey := types.NamespacedName{Name: webName, Namespace: ns}
			live := &appsv1.Deployment{}
			Eventually(func() error {
				return k8sClient.Get(context.Background(), webKey, live)
			}, "30s").Should(Succeed(), "web deployment should be created")

			// Diagnostic, not an assertion: the fields the builders omit and the
			// API server defaults. Under a full-spec DeepEqual these made the
			// change-detection gate open on every pass; server-side apply never
			// looks at them, because the operator does not declare them.
			Expect(k8sClient.Get(context.Background(),
				types.NamespacedName{Name: name, Namespace: ns}, instance)).To(Succeed())
			config, err := langfuse.BuildConfig(instance)
			Expect(err).NotTo(HaveOccurred())
			diffs := specDifferences(live.Spec, resources.BuildWebDeployment(instance, config).Spec)
			AddReportEntry("h5-defaulted-fields", strings.Join(diffs, "\n"))
			GinkgoWriter.Printf("\n── %d fields the builder omits that the API server defaults ──\n  %s\n",
				len(diffs), strings.Join(diffs, "\n  "))

			// Stand in for a sibling controller claiming a field the operator does
			// not declare — this is the pod-template annotation SecretController
			// writes to trigger rolling restarts.
			sibling := &unstructured.Unstructured{}
			sibling.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("Deployment"))
			sibling.SetNamespace(ns)
			sibling.SetName(webName)
			Expect(unstructured.SetNestedStringMap(sibling.Object,
				map[string]string{secretHashAnnotation: "sibling-owned"},
				"spec", "template", "metadata", "annotations")).To(Succeed())
			Expect(k8sClient.Patch(context.Background(), sibling, client.Apply,
				client.FieldOwner("sibling-controller"), client.ForceOwnership)).To(Succeed())

			// Drive the operator through repeated reconciles.
			observeChurn(types.NamespacedName{Name: name, Namespace: ns}, churnOpts{
				window: 8 * time.Second, poke: true, pokeInterval: time.Second,
				deployments: []string{webName},
			})

			Expect(k8sClient.Get(context.Background(), webKey, live)).To(Succeed())
			Expect(live.Spec.Template.Annotations).To(HaveKeyWithValue(secretHashAnnotation, "sibling-owned"),
				"the operator overwrote a pod-template annotation it does not own")
		})

		It("ignores status-only writes to an owned HPA", func() {
			ns := newChurnNamespace(namespace + "-hpa")

			startChurnManager(func(mgr ctrl.Manager) error {
				return (&LangfuseInstanceReconciler{
					Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
				}).SetupWithManager(mgr)
			})

			instance := newChurnInstance(ns, name)
			instance.Spec.Web.Autoscaling = &v1alpha1.AutoscalingSpec{
				Enabled:              true,
				MinReplicas:          ptr.To(int32(1)),
				MaxReplicas:          3,
				TargetCPUUtilization: ptr.To(int32(80)),
			}
			Expect(k8sClient.Create(context.Background(), instance)).To(Succeed())

			hpaKey := types.NamespacedName{Name: resources.WebName(instance), Namespace: ns}
			Eventually(func() error {
				return k8sClient.Get(context.Background(), hpaKey, &autoscalingv2.HorizontalPodAutoscaler{})
			}, "30s").Should(Succeed(), "web HPA should be created")

			// Let the initial burst finish, then stand in for the real HPA
			// controller, which rewrites status roughly every 15s. Owns() carries
			// no predicate, so these status-only writes reach the instance
			// controller as full reconcile requests.
			time.Sleep(churnSettle)
			before := reconcileTotals()["langfuseinstance"]

			const statusWrites = 5
			for i := 0; i < statusWrites; i++ {
				hpa := &autoscalingv2.HorizontalPodAutoscaler{}
				Expect(k8sClient.Get(context.Background(), hpaKey, hpa)).To(Succeed())
				hpa.Status.CurrentReplicas = int32(i % 2)
				hpa.Status.DesiredReplicas = int32(i%2) + 1
				Expect(k8sClient.Status().Update(context.Background(), hpa)).To(Succeed())
				time.Sleep(500 * time.Millisecond)
			}
			time.Sleep(2 * time.Second)

			triggered := reconcileTotals()["langfuseinstance"] - before
			GinkgoWriter.Printf("\n── %d HPA status-only writes triggered %.0f instance reconciles ──\n",
				statusWrites, triggered)
			AddReportEntry("h5-hpa-status-writes",
				fmt.Sprintf("%d HPA status writes → %.0f instance reconciles", statusWrites, triggered))

			Expect(triggered).To(BeNumerically("==", 0),
				"Owns(&HorizontalPodAutoscaler{}) has no predicate, so every HPA status "+
					"rewrite (~4/min per instance in a real cluster) runs a full instance reconcile")
		})
	})

	// ─── H4: circuit breaker vs. the instance controller's replicas ──────────

	Describe("H4: instance controller + circuit breaker", func() {
		const (
			namespace = "churn-h4"
			name      = "churn"
		)

		It("keeps the worker scaled to zero once the breaker trips", func() {
			ns := newChurnNamespace(namespace)

			startChurnManager(
				func(mgr ctrl.Manager) error {
					return (&LangfuseInstanceReconciler{
						Client: mgr.GetClient(), Scheme: mgr.GetScheme(),
					}).SetupWithManager(mgr)
				},
				func(mgr ctrl.Manager) error {
					return (&CircuitBreakerController{
						Client:   mgr.GetClient(),
						Scheme:   mgr.GetScheme(),
						Recorder: mgr.GetEventRecorderFor("circuit-breaker"),
					}).SetupWithManager(mgr)
				},
			)

			instance := newChurnInstance(ns, name)
			instance.Spec.Worker.Replicas = ptr.To(int32(2))
			instance.Spec.CircuitBreaker = &v1alpha1.CircuitBreakerSpec{
				Enabled: ptr.To(true),
				ClickHouse: &v1alpha1.ComponentCircuitBreakerSpec{
					Action:               "scaleWorkerToZero",
					ProbeIntervalSeconds: 1,
					FailureThreshold:     1,
					RecoveryAction:       "restoreScale",
				},
			}
			Expect(k8sClient.Create(context.Background(), instance)).To(Succeed())

			workerName := resources.WorkerName(instance)
			Eventually(func() error {
				return k8sClient.Get(context.Background(),
					types.NamespacedName{Name: workerName, Namespace: ns}, &appsv1.Deployment{})
			}, "30s").Should(Succeed(), "worker deployment should be created")

			report := observeChurn(types.NamespacedName{Name: name, Namespace: ns}, churnOpts{
				deployments: []string{resources.WebName(instance), workerName},
			})

			// ClickHouse is unreachable, so the breaker opens and scales the
			// worker to 0. It must stay there: the instance controller's
			// reconcileDeployment does `existing.Spec = desired.Spec` from a
			// builder that always sets Replicas from the CR, and updateStatus
			// replaces the whole Status.Worker struct, wiping
			// CircuitBreakerActive so the breaker cannot tell it already tripped.
			Expect(report.replicas[workerName]).To(HaveLen(1),
				"worker spec.replicas oscillated between the breaker and the instance controller: %v",
				report.replicas[workerName])
			Expect(report.replicas[workerName][0]).To(BeNumerically("==", 0),
				"worker should remain scaled to zero while the breaker is open")
		})
	})
})
