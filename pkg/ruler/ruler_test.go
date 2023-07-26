package ruler

import (
	"context"
	"fmt"
	io "io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/thanos-io/objstore"

	"github.com/cortexproject/cortex/pkg/ruler/rulestore/bucketclient"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/gorilla/mux"
	"github.com/prometheus/client_golang/prometheus"
	prom_testutil "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/model/rulefmt"
	"github.com/prometheus/prometheus/notifier"
	"github.com/prometheus/prometheus/promql"
	promRules "github.com/prometheus/prometheus/rules"
	"github.com/prometheus/prometheus/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"github.com/weaveworks/common/user"
	"go.uber.org/atomic"
	"google.golang.org/grpc"
	"gopkg.in/yaml.v3"

	"github.com/cortexproject/cortex/pkg/cortexpb"
	"github.com/cortexproject/cortex/pkg/purger"
	"github.com/cortexproject/cortex/pkg/querier"
	"github.com/cortexproject/cortex/pkg/ring"
	"github.com/cortexproject/cortex/pkg/ring/kv"
	"github.com/cortexproject/cortex/pkg/ring/kv/consul"
	"github.com/cortexproject/cortex/pkg/ruler/rulespb"
	"github.com/cortexproject/cortex/pkg/ruler/rulestore"
	"github.com/cortexproject/cortex/pkg/tenant"
	"github.com/cortexproject/cortex/pkg/util"
	"github.com/cortexproject/cortex/pkg/util/flagext"
	"github.com/cortexproject/cortex/pkg/util/services"
	"github.com/cortexproject/cortex/pkg/util/validation"
)

func defaultRulerConfig(t testing.TB) Config {
	t.Helper()

	// Create a new temporary directory for the rules, so that
	// each test will run in isolation.
	rulesDir := t.TempDir()

	codec := ring.GetCodec()
	consul, closer := consul.NewInMemoryClient(codec, log.NewNopLogger(), nil)
	t.Cleanup(func() { assert.NoError(t, closer.Close()) })

	cfg := Config{}
	flagext.DefaultValues(&cfg)
	cfg.RulePath = rulesDir
	cfg.Ring.KVStore.Mock = consul
	cfg.Ring.NumTokens = 1
	cfg.Ring.ListenPort = 0
	cfg.Ring.InstanceAddr = "localhost"
	cfg.Ring.InstanceID = "localhost"
	cfg.EnableQueryStats = false

	return cfg
}

type ruleLimits struct {
	evalDelay            time.Duration
	tenantShard          int
	maxRulesPerRuleGroup int
	maxRuleGroups        int
}

func (r ruleLimits) EvaluationDelay(_ string) time.Duration {
	return r.evalDelay
}

func (r ruleLimits) RulerTenantShardSize(_ string) int {
	return r.tenantShard
}

func (r ruleLimits) RulerMaxRuleGroupsPerTenant(_ string) int {
	return r.maxRuleGroups
}

func (r ruleLimits) RulerMaxRulesPerRuleGroup(_ string) int {
	return r.maxRulesPerRuleGroup
}

func newEmptyQueryable() storage.Queryable {
	return storage.QueryableFunc(func(ctx context.Context, mint, maxt int64) (storage.Querier, error) {
		return emptyQuerier{}, nil
	})
}

type emptyQuerier struct {
}

func (e emptyQuerier) LabelValues(name string, matchers ...*labels.Matcher) ([]string, storage.Warnings, error) {
	return nil, nil, nil
}

func (e emptyQuerier) LabelNames(matchers ...*labels.Matcher) ([]string, storage.Warnings, error) {
	return nil, nil, nil
}

func (e emptyQuerier) Close() error {
	return nil
}

func (e emptyQuerier) Select(sortSeries bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	return storage.EmptySeriesSet()
}

func testQueryableFunc(querierTestConfig *querier.TestConfig, reg prometheus.Registerer, logger log.Logger) storage.QueryableFunc {
	if querierTestConfig != nil {
		// disable active query tracking for test
		querierTestConfig.Cfg.ActiveQueryTrackerDir = ""

		overrides, _ := validation.NewOverrides(querier.DefaultLimitsConfig(), nil)
		q, _, _ := querier.New(querierTestConfig.Cfg, overrides, querierTestConfig.Distributor, querierTestConfig.Stores, purger.NewNoopTombstonesLoader(), reg, logger)
		return func(ctx context.Context, mint, maxt int64) (storage.Querier, error) {
			return q.Querier(ctx, mint, maxt)
		}
	}

	return func(ctx context.Context, mint, maxt int64) (storage.Querier, error) {
		return storage.NoopQuerier(), nil
	}
}

func testSetup(t *testing.T, querierTestConfig *querier.TestConfig) (*promql.Engine, storage.QueryableFunc, Pusher, log.Logger, RulesLimits, prometheus.Registerer) {
	tracker := promql.NewActiveQueryTracker(t.TempDir(), 20, log.NewNopLogger())

	engine := promql.NewEngine(promql.EngineOpts{
		MaxSamples:         1e6,
		ActiveQueryTracker: tracker,
		Timeout:            2 * time.Minute,
	})

	// Mock the pusher
	pusher := newPusherMock()
	pusher.MockPush(&cortexpb.WriteResponse{}, nil)

	l := log.NewLogfmtLogger(os.Stdout)
	l = level.NewFilter(l, level.AllowInfo())

	reg := prometheus.NewRegistry()
	queryable := testQueryableFunc(querierTestConfig, reg, l)

	return engine, queryable, pusher, l, ruleLimits{evalDelay: 0, maxRuleGroups: 20, maxRulesPerRuleGroup: 15}, reg
}

func newManager(t *testing.T, cfg Config) *DefaultMultiTenantManager {
	engine, queryable, pusher, logger, overrides, reg := testSetup(t, nil)
	manager, err := NewDefaultMultiTenantManager(cfg, DefaultTenantManagerFactory(cfg, pusher, queryable, engine, overrides, nil), reg, logger)
	require.NoError(t, err)

	return manager
}

type mockRulerClientsPool struct {
	ClientsPool
	cfg           Config
	rulerAddrMap  map[string]*Ruler
	numberOfCalls atomic.Int32
}

type mockRulerClient struct {
	ruler         *Ruler
	numberOfCalls *atomic.Int32
}

func (c *mockRulerClient) Rules(ctx context.Context, in *RulesRequest, _ ...grpc.CallOption) (*RulesResponse, error) {
	c.numberOfCalls.Inc()
	return c.ruler.Rules(ctx, in)
}

func (p *mockRulerClientsPool) GetClientFor(addr string) (RulerClient, error) {
	for _, r := range p.rulerAddrMap {
		if r.lifecycler.GetInstanceAddr() == addr {
			return &mockRulerClient{
				ruler:         r,
				numberOfCalls: &p.numberOfCalls,
			}, nil
		}
	}

	return nil, fmt.Errorf("unable to find ruler for add %s", addr)
}

func newMockClientsPool(cfg Config, logger log.Logger, reg prometheus.Registerer, rulerAddrMap map[string]*Ruler) *mockRulerClientsPool {
	return &mockRulerClientsPool{
		ClientsPool:  newRulerClientPool(cfg.ClientTLSConfig, logger, reg),
		cfg:          cfg,
		rulerAddrMap: rulerAddrMap,
	}
}

func buildRuler(t *testing.T, rulerConfig Config, querierTestConfig *querier.TestConfig, store rulestore.RuleStore, rulerAddrMap map[string]*Ruler) *Ruler {
	engine, queryable, pusher, logger, overrides, reg := testSetup(t, querierTestConfig)

	managerFactory := DefaultTenantManagerFactory(rulerConfig, pusher, queryable, engine, overrides, reg)
	manager, err := NewDefaultMultiTenantManager(rulerConfig, managerFactory, reg, log.NewNopLogger())
	require.NoError(t, err)

	ruler, err := newRuler(
		rulerConfig,
		manager,
		reg,
		logger,
		store,
		overrides,
		newMockClientsPool(rulerConfig, logger, reg, rulerAddrMap),
	)
	require.NoError(t, err)
	return ruler
}

func newTestRuler(t *testing.T, rulerConfig Config, store rulestore.RuleStore, querierTestConfig *querier.TestConfig) *Ruler {
	ruler := buildRuler(t, rulerConfig, querierTestConfig, store, nil)
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), ruler))

	// Ensure all rules are loaded before usage
	ruler.syncRules(context.Background(), rulerSyncReasonInitial)

	return ruler
}

var _ MultiTenantManager = &DefaultMultiTenantManager{}

func TestNotifierSendsUserIDHeader(t *testing.T) {
	var wg sync.WaitGroup

	// We do expect 1 API call for the user create with the getOrCreateNotifier()
	wg.Add(1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, _, err := tenant.ExtractTenantIDFromHTTPRequest(r)
		assert.NoError(t, err)
		assert.Equal(t, userID, "1")
		wg.Done()
	}))
	defer ts.Close()

	cfg := defaultRulerConfig(t)

	cfg.AlertmanagerURL = ts.URL
	cfg.AlertmanagerDiscovery = false

	manager := newManager(t, cfg)
	defer manager.Stop()

	n, err := manager.getOrCreateNotifier("1", manager.registry)
	require.NoError(t, err)

	// Loop until notifier discovery syncs up
	for len(n.Alertmanagers()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	n.Send(&notifier.Alert{
		Labels: labels.Labels{labels.Label{Name: "alertname", Value: "testalert"}},
	})

	wg.Wait()

	// Ensure we have metrics in the notifier.
	assert.NoError(t, prom_testutil.GatherAndCompare(manager.registry.(*prometheus.Registry), strings.NewReader(`
		# HELP prometheus_notifications_dropped_total Total number of alerts dropped due to errors when sending to Alertmanager.
		# TYPE prometheus_notifications_dropped_total counter
		prometheus_notifications_dropped_total 0
	`), "prometheus_notifications_dropped_total"))
}

func TestRuler_Rules(t *testing.T) {
	store := newMockRuleStore(mockRules)
	cfg := defaultRulerConfig(t)

	r := newTestRuler(t, cfg, store, nil)
	defer services.StopAndAwaitTerminated(context.Background(), r) //nolint:errcheck

	// test user1
	ctx := user.InjectOrgID(context.Background(), "user1")
	rls, err := r.Rules(ctx, &RulesRequest{})
	require.NoError(t, err)
	require.Len(t, rls.Groups, 1)
	rg := rls.Groups[0]
	expectedRg := mockRules["user1"][0]
	compareRuleGroupDescToStateDesc(t, expectedRg, rg)

	// test user2
	ctx = user.InjectOrgID(context.Background(), "user2")
	rls, err = r.Rules(ctx, &RulesRequest{})
	require.NoError(t, err)
	require.Len(t, rls.Groups, 1)
	rg = rls.Groups[0]
	expectedRg = mockRules["user2"][0]
	compareRuleGroupDescToStateDesc(t, expectedRg, rg)
}

func compareRuleGroupDescToStateDesc(t *testing.T, expected *rulespb.RuleGroupDesc, got *GroupStateDesc) {
	require.Equal(t, got.Group.Name, expected.Name)
	require.Equal(t, got.Group.Namespace, expected.Namespace)
	require.Len(t, expected.Rules, len(got.ActiveRules))
	for i := range got.ActiveRules {
		require.Equal(t, expected.Rules[i].Record, got.ActiveRules[i].Rule.Record)
		require.Equal(t, expected.Rules[i].Alert, got.ActiveRules[i].Rule.Alert)
	}
}

func TestGetRules(t *testing.T) {
	// ruler ID -> (user ID -> list of groups).
	type expectedRulesMap map[string]map[string]rulespb.RuleGroupList
	type rulesMap map[string][]*rulespb.RuleDesc

	type testCase struct {
		sharding          bool
		shardingStrategy  string
		shuffleShardSize  int
		replicationFactor int
		rulesRequest      RulesRequest
		expectedCount     map[string]int
	}

	ruleMap := rulesMap{
		"ruler1-user1-rule-group1": []*rulespb.RuleDesc{
			{
				Record: "rtest_user1_1",
				Expr:   "sum(rate(node_cpu_seconds_total[3h:10m]))",
			},
			{
				Alert: "atest_user1_1",
				Expr:  "sum(rate(node_cpu_seconds_total[3h:10m]))",
			},
		},
		"ruler1-user1-rule-group2": []*rulespb.RuleDesc{
			{
				Record: "rtest_user1_1",
				Expr:   "sum(rate(node_cpu_seconds_total[3h:10m]))",
			},
		},
		"ruler1-user2-rule-group1": []*rulespb.RuleDesc{
			{
				Record: "rtest_user1_1",
				Expr:   "sum(rate(node_cpu_seconds_total[3h:10m]))",
			},
		},
		"ruler2-user1-rule-group3": []*rulespb.RuleDesc{
			{
				Record: "rtest_user1_1",
				Expr:   "sum(rate(node_cpu_seconds_total[3h:10m]))",
			},
			{
				Alert: "atest_user1_1",
				Expr:  "sum(rate(node_cpu_seconds_total[3h:10m]))",
			},
		},
		"ruler2-user2-rule-group1": []*rulespb.RuleDesc{
			{
				Record: "rtest_user1_1",
				Expr:   "sum(rate(node_cpu_seconds_total[3h:10m]))",
			},
			{
				Alert: "atest_user1_1",
				Expr:  "sum(rate(node_cpu_seconds_total[3h:10m]))",
			},
		},
		"ruler2-user2-rule-group2": []*rulespb.RuleDesc{
			{
				Record: "rtest_user2_1",
				Expr:   "sum(rate(node_cpu_seconds_total[3h:10m]))",
			},
			{
				Alert: "atest_user2_1",
				Expr:  "sum(rate(node_cpu_seconds_total[3h:10m]))",
			},
		},
		"ruler2-user3-rule-group1": []*rulespb.RuleDesc{
			{
				Alert: "atest_user3_1",
				Expr:  "sum(rate(node_cpu_seconds_total[3h:10m]))",
			},
		},
		"ruler3-user2-rule-group1": []*rulespb.RuleDesc{
			{
				Record: "rtest_user1_1",
				Expr:   "sum(rate(node_cpu_seconds_total[3h:10m]))",
			},
			{
				Alert: "atest_user1_1",
				Expr:  "sum(rate(node_cpu_seconds_total[3h:10m]))",
			},
		},
		"ruler3-user2-rule-group2": []*rulespb.RuleDesc{
			{
				Record: "rtest_user1_1",
				Expr:   "sum(rate(node_cpu_seconds_total[3h:10m]))",
			},
			{
				Alert: "atest_user1_1",
				Expr:  "sum(rate(node_cpu_seconds_total[3h:10m]))",
			},
		},
		"ruler3-user3-rule-group1": []*rulespb.RuleDesc{
			{
				Expr:   "sum(rate(node_cpu_seconds_total[3h:10m]))",
				Record: "rtest_user1_1",
			},
			{
				Alert: "atest_user1_1",
				Expr:  "sum(rate(node_cpu_seconds_total[3h:10m]))",
			},
		},
	}

	expectedRules := expectedRulesMap{
		"ruler1": map[string]rulespb.RuleGroupList{
			"user1": {
				&rulespb.RuleGroupDesc{User: "user1", Namespace: "namespace", Name: "first", Interval: 10 * time.Second, Rules: ruleMap["ruler1-user1-rule-group1"]},
				&rulespb.RuleGroupDesc{User: "user1", Namespace: "namespace", Name: "second", Interval: 10 * time.Second, Rules: ruleMap["ruler1-user1-rule-group2"]},
			},
			"user2": {
				&rulespb.RuleGroupDesc{User: "user2", Namespace: "namespace", Name: "third", Interval: 10 * time.Second, Rules: ruleMap["ruler1-user2-rule-group1"]},
			},
		},
		"ruler2": map[string]rulespb.RuleGroupList{
			"user1": {
				&rulespb.RuleGroupDesc{User: "user1", Namespace: "namespace", Name: "third", Interval: 10 * time.Second, Rules: ruleMap["ruler2-user1-rule-group3"]},
			},
			"user2": {
				&rulespb.RuleGroupDesc{User: "user2", Namespace: "namespace", Name: "first", Interval: 10 * time.Second, Rules: ruleMap["ruler2-user2-rule-group1"]},
				&rulespb.RuleGroupDesc{User: "user2", Namespace: "namespace", Name: "second", Interval: 10 * time.Second, Rules: ruleMap["ruler2-user2-rule-group2"]},
			},
			"user3": {
				&rulespb.RuleGroupDesc{User: "user3", Namespace: "latency-test", Name: "first", Interval: 10 * time.Second, Rules: ruleMap["ruler2-user3-rule-group1"]},
			},
		},
		"ruler3": map[string]rulespb.RuleGroupList{
			"user3": {
				&rulespb.RuleGroupDesc{User: "user3", Namespace: "namespace", Name: "third", Interval: 10 * time.Second, Rules: ruleMap["ruler3-user3-rule-group1"]},
			},
			"user2": {
				&rulespb.RuleGroupDesc{User: "user2", Namespace: "namespace", Name: "forth", Interval: 10 * time.Second, Rules: ruleMap["ruler3-user2-rule-group1"]},
				&rulespb.RuleGroupDesc{User: "user2", Namespace: "namespace", Name: "fifty", Interval: 10 * time.Second, Rules: ruleMap["ruler3-user2-rule-group2"]},
			},
		},
	}

	testCases := map[string]testCase{
		"No Sharding with Rule Type Filter": {
			sharding: false,
			rulesRequest: RulesRequest{
				Type: alertingRuleFilter,
			},
			expectedCount: map[string]int{
				"user1": 2,
				"user2": 4,
				"user3": 2,
			},
		},
		"Default Sharding with No Filter": {
			sharding:         true,
			shardingStrategy: util.ShardingStrategyDefault,
			expectedCount: map[string]int{
				"user1": 5,
				"user2": 9,
				"user3": 3,
			},
		},
		"Default Sharding and replicationFactor = 3 with No Filter": {
			sharding:          true,
			shardingStrategy:  util.ShardingStrategyDefault,
			replicationFactor: 3,
			expectedCount: map[string]int{
				"user1": 5,
				"user2": 9,
				"user3": 3,
			},
		},
		"Shuffle Sharding and ShardSize = 2 with Rule Type Filter": {
			sharding:         true,
			shuffleShardSize: 2,
			shardingStrategy: util.ShardingStrategyShuffle,
			rulesRequest: RulesRequest{
				Type: recordingRuleFilter,
			},
			expectedCount: map[string]int{
				"user1": 3,
				"user2": 5,
				"user3": 1,
			},
		},
		"Shuffle Sharding and ShardSize = 2 and Rule Group Name Filter": {
			sharding:         true,
			shuffleShardSize: 2,
			shardingStrategy: util.ShardingStrategyShuffle,
			rulesRequest: RulesRequest{
				RuleGroupNames: []string{"third"},
			},
			expectedCount: map[string]int{
				"user1": 2,
				"user2": 1,
				"user3": 2,
			},
		},
		"Shuffle Sharding and ShardSize = 2 and Rule Group Name and Rule Type Filter": {
			sharding:         true,
			shuffleShardSize: 2,
			shardingStrategy: util.ShardingStrategyShuffle,
			rulesRequest: RulesRequest{
				RuleGroupNames: []string{"second", "third"},
				Type:           recordingRuleFilter,
			},
			expectedCount: map[string]int{
				"user1": 2,
				"user2": 2,
				"user3": 1,
			},
		},
		"Shuffle Sharding and ShardSize = 2 with Rule Type and Namespace Filters": {
			sharding:         true,
			shuffleShardSize: 2,
			shardingStrategy: util.ShardingStrategyShuffle,
			rulesRequest: RulesRequest{
				Type:  alertingRuleFilter,
				Files: []string{"latency-test"},
			},
			expectedCount: map[string]int{
				"user1": 0,
				"user2": 0,
				"user3": 1,
			},
		},
		"Shuffle Sharding and ShardSize = 3 and replicationFactor = 3 with No Filter": {
			sharding:          true,
			shardingStrategy:  util.ShardingStrategyShuffle,
			shuffleShardSize:  3,
			replicationFactor: 3,
			expectedCount: map[string]int{
				"user1": 5,
				"user2": 9,
				"user3": 3,
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			kvStore, cleanUp := consul.NewInMemoryClient(ring.GetCodec(), log.NewNopLogger(), nil)
			t.Cleanup(func() { assert.NoError(t, cleanUp.Close()) })
			allRulesByUser := map[string]rulespb.RuleGroupList{}
			allRulesByRuler := map[string]rulespb.RuleGroupList{}
			allTokensByRuler := map[string][]uint32{}
			rulerAddrMap := map[string]*Ruler{}

			createRuler := func(id string) *Ruler {
				store := newMockRuleStore(allRulesByUser)
				cfg := defaultRulerConfig(t)

				cfg.ShardingStrategy = tc.shardingStrategy
				cfg.EnableSharding = tc.sharding

				rulerReplicationFactor := tc.replicationFactor
				if rulerReplicationFactor == 0 {
					rulerReplicationFactor = 1
				}

				cfg.Ring = RingConfig{
					InstanceID:   id,
					InstanceAddr: id,
					KVStore: kv.Config{
						Mock: kvStore,
					},
					ReplicationFactor: rulerReplicationFactor,
				}

				r := buildRuler(t, cfg, nil, store, rulerAddrMap)
				r.limits = ruleLimits{evalDelay: 0, tenantShard: tc.shuffleShardSize}
				rulerAddrMap[id] = r
				if r.ring != nil {
					require.NoError(t, services.StartAndAwaitRunning(context.Background(), r.ring))
					t.Cleanup(r.ring.StopAsync)
				}
				return r
			}

			for rID, r := range expectedRules {
				createRuler(rID)
				for user, rules := range r {
					allRulesByUser[user] = append(allRulesByUser[user], rules...)
					allRulesByRuler[rID] = append(allRulesByRuler[rID], rules...)
					allTokensByRuler[rID] = generateTokenForGroups(rules, 1)
				}
			}

			if tc.sharding {
				err := kvStore.CAS(context.Background(), ringKey, func(in interface{}) (out interface{}, retry bool, err error) {
					d, _ := in.(*ring.Desc)
					if d == nil {
						d = ring.NewDesc()
					}
					for rID, tokens := range allTokensByRuler {
						d.AddIngester(rID, rulerAddrMap[rID].lifecycler.GetInstanceAddr(), "", tokens, ring.ACTIVE, time.Now())
					}
					return d, true, nil
				})
				require.NoError(t, err)
				// Wait a bit to make sure ruler's ring is updated.
				time.Sleep(100 * time.Millisecond)
			}

			forEachRuler := func(f func(rID string, r *Ruler)) {
				for rID, r := range rulerAddrMap {
					f(rID, r)
				}
			}

			// Sync Rules
			forEachRuler(func(_ string, r *Ruler) {
				r.syncRules(context.Background(), rulerSyncReasonInitial)
			})
			for u := range allRulesByUser {
				ctx := user.InjectOrgID(context.Background(), u)
				forEachRuler(func(_ string, r *Ruler) {
					ruleStateDescriptions, err := r.GetRules(ctx, tc.rulesRequest)
					require.NoError(t, err)
					rct := 0
					for _, ruleStateDesc := range ruleStateDescriptions {
						rct += len(ruleStateDesc.ActiveRules)
					}
					require.Equal(t, tc.expectedCount[u], rct)
					// If replication factor larger than 1, we don't necessary need to wait for all ruler's call to complete to get the
					// complete result
					if tc.sharding && tc.replicationFactor <= 1 {
						mockPoolClient := r.clientsPool.(*mockRulerClientsPool)

						if tc.shardingStrategy == util.ShardingStrategyShuffle {
							require.Equal(t, int32(tc.shuffleShardSize), mockPoolClient.numberOfCalls.Load())
						} else {
							require.Equal(t, int32(len(rulerAddrMap)), mockPoolClient.numberOfCalls.Load())
						}
						mockPoolClient.numberOfCalls.Store(0)
					}
				})
			}

			totalLoadedRules := 0
			totalConfiguredRules := 0

			forEachRuler(func(rID string, r *Ruler) {
				localRules, err := r.listRules(context.Background())
				require.NoError(t, err)
				for _, rules := range localRules {
					totalLoadedRules += len(rules)
				}
				totalConfiguredRules += len(allRulesByRuler[rID])
			})

			if tc.sharding && tc.replicationFactor <= 1 {
				require.Equal(t, totalConfiguredRules, totalLoadedRules)
			} else if tc.replicationFactor > 1 {
				require.Equal(t, totalConfiguredRules*tc.replicationFactor, totalLoadedRules)
			} else {
				// Not sharding means that all rules will be loaded on all rulers
				numberOfRulers := len(rulerAddrMap)
				require.Equal(t, totalConfiguredRules*numberOfRulers, totalLoadedRules)
			}
		})
	}
}

func TestSharding(t *testing.T) {
	const (
		user1 = "user1"
		user2 = "user2"
		user3 = "user3"
		zone1 = "zone1"
		zone2 = "zone2"
		zone3 = "zone3"
	)

	user1Group1 := &rulespb.RuleGroupDesc{User: user1, Namespace: "namespace", Name: "first"}
	user1Group2 := &rulespb.RuleGroupDesc{User: user1, Namespace: "namespace", Name: "second"}
	user2Group1 := &rulespb.RuleGroupDesc{User: user2, Namespace: "namespace", Name: "first"}
	user3Group1 := &rulespb.RuleGroupDesc{User: user3, Namespace: "namespace", Name: "first"}

	// Must be distinct for test to work.
	user1Group1Token := tokenForGroup(user1Group1)
	user1Group2Token := tokenForGroup(user1Group2)
	user2Group1Token := tokenForGroup(user2Group1)
	user3Group1Token := tokenForGroup(user3Group1)

	noRules := map[string]rulespb.RuleGroupList{}
	allRules := map[string]rulespb.RuleGroupList{
		user1: {user1Group1, user1Group2},
		user2: {user2Group1},
		user3: {user3Group1},
	}

	// ruler ID -> (user ID -> list of groups).
	type expectedRulesMap map[string]map[string]rulespb.RuleGroupList

	type testCase struct {
		sharding          bool
		shardingStrategy  string
		shuffleShardSize  int
		replicationFactor int
		zoneAwareness     bool
		setupRing         func(*ring.Desc)
		enabledUsers      []string
		disabledUsers     []string

		expectedRules expectedRulesMap
	}

	type rulerDesc struct {
		id   string
		host string
		port int
		addr string
		zone string
	}
	newRulerDesc := func(index int, zone string) rulerDesc {
		host := fmt.Sprintf("%d.%d.%d.%d", index, index, index, index)
		port := 9999
		return rulerDesc{
			id:   fmt.Sprintf("ruler-%d", index),
			host: host,
			port: port,
			addr: fmt.Sprintf("%s:%d", host, port),
			zone: zone,
		}
	}
	rulerDescs := [...]rulerDesc{
		newRulerDesc(1, zone1),
		newRulerDesc(2, zone2),
		newRulerDesc(3, zone3),
		newRulerDesc(4, zone1),
		newRulerDesc(5, zone2),
		newRulerDesc(6, zone3),
	}

	testCases := map[string]testCase{
		"0 no sharding": {
			sharding:      false,
			expectedRules: expectedRulesMap{rulerDescs[0].id: allRules},
		},

		"1 no sharding, single user allowed": {
			sharding:     false,
			enabledUsers: []string{user1},
			expectedRules: expectedRulesMap{rulerDescs[0].id: map[string]rulespb.RuleGroupList{
				user1: {user1Group1, user1Group2},
			}},
		},

		"2 no sharding, single user disabled": {
			sharding:      false,
			disabledUsers: []string{user1},
			expectedRules: expectedRulesMap{rulerDescs[0].id: map[string]rulespb.RuleGroupList{
				user2: {user2Group1},
				user3: {user3Group1},
			}},
		},

		"3 default sharding, single ruler": {
			sharding:         true,
			shardingStrategy: util.ShardingStrategyDefault,
			setupRing: func(desc *ring.Desc) {
				desc.AddIngester(rulerDescs[0].id, rulerDescs[0].addr, rulerDescs[0].zone, []uint32{0}, ring.ACTIVE, time.Now())
			},
			expectedRules: expectedRulesMap{rulerDescs[0].id: allRules},
		},

		"4 default sharding, single ruler, single enabled user": {
			sharding:         true,
			shardingStrategy: util.ShardingStrategyDefault,
			enabledUsers:     []string{user1},
			setupRing: func(desc *ring.Desc) {
				desc.AddIngester(rulerDescs[0].id, rulerDescs[0].addr, rulerDescs[0].zone, []uint32{0}, ring.ACTIVE, time.Now())
			},
			expectedRules: expectedRulesMap{rulerDescs[0].id: map[string]rulespb.RuleGroupList{
				user1: {user1Group1, user1Group2},
			}},
		},

		"5 default sharding, single ruler, single disabled user": {
			sharding:         true,
			shardingStrategy: util.ShardingStrategyDefault,
			disabledUsers:    []string{user1},
			setupRing: func(desc *ring.Desc) {
				desc.AddIngester(rulerDescs[0].id, rulerDescs[0].addr, rulerDescs[0].zone, []uint32{0}, ring.ACTIVE, time.Now())
			},
			expectedRules: expectedRulesMap{rulerDescs[0].id: map[string]rulespb.RuleGroupList{
				user2: {user2Group1},
				user3: {user3Group1},
			}},
		},

		"6 default sharding, multiple ACTIVE rulerDescs": {
			sharding:         true,
			shardingStrategy: util.ShardingStrategyDefault,
			setupRing: func(desc *ring.Desc) {
				desc.AddIngester(rulerDescs[0].id, rulerDescs[0].addr, rulerDescs[0].zone, sortTokens([]uint32{user1Group1Token + 1, user2Group1Token + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[1].id, rulerDescs[1].addr, rulerDescs[1].zone, sortTokens([]uint32{user1Group2Token + 1, user3Group1Token + 1}), ring.ACTIVE, time.Now())
			},

			expectedRules: expectedRulesMap{
				rulerDescs[0].id: map[string]rulespb.RuleGroupList{
					user1: {user1Group1},
					user2: {user2Group1},
				},

				rulerDescs[1].id: map[string]rulespb.RuleGroupList{
					user1: {user1Group2},
					user3: {user3Group1},
				},
			},
		},

		"7 default sharding, multiple ACTIVE rulerDescs, single enabled user": {
			sharding:         true,
			shardingStrategy: util.ShardingStrategyDefault,
			enabledUsers:     []string{user1},
			setupRing: func(desc *ring.Desc) {
				desc.AddIngester(rulerDescs[0].id, rulerDescs[0].addr, rulerDescs[0].zone, sortTokens([]uint32{user1Group1Token + 1, user2Group1Token + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[1].id, rulerDescs[1].addr, rulerDescs[1].zone, sortTokens([]uint32{user1Group2Token + 1, user3Group1Token + 1}), ring.ACTIVE, time.Now())
			},

			expectedRules: expectedRulesMap{
				rulerDescs[0].id: map[string]rulespb.RuleGroupList{
					user1: {user1Group1},
				},

				rulerDescs[1].id: map[string]rulespb.RuleGroupList{
					user1: {user1Group2},
				},
			},
		},

		"8 default sharding, multiple ACTIVE rulerDescs, single disabled user": {
			sharding:         true,
			shardingStrategy: util.ShardingStrategyDefault,
			disabledUsers:    []string{user1},
			setupRing: func(desc *ring.Desc) {
				desc.AddIngester(rulerDescs[0].id, rulerDescs[0].addr, rulerDescs[0].zone, sortTokens([]uint32{user1Group1Token + 1, user2Group1Token + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[1].id, rulerDescs[1].addr, rulerDescs[1].zone, sortTokens([]uint32{user1Group2Token + 1, user3Group1Token + 1}), ring.ACTIVE, time.Now())
			},

			expectedRules: expectedRulesMap{
				rulerDescs[0].id: map[string]rulespb.RuleGroupList{
					user2: {user2Group1},
				},

				rulerDescs[1].id: map[string]rulespb.RuleGroupList{
					user3: {user3Group1},
				},
			},
		},

		"9 default sharding, unhealthy ACTIVE ruler": {
			sharding:         true,
			shardingStrategy: util.ShardingStrategyDefault,

			setupRing: func(desc *ring.Desc) {
				desc.AddIngester(rulerDescs[0].id, rulerDescs[0].addr, rulerDescs[0].zone, sortTokens([]uint32{user1Group1Token + 1, user2Group1Token + 1}), ring.ACTIVE, time.Now())
				desc.Ingesters[rulerDescs[1].id] = ring.InstanceDesc{
					Addr:      rulerDescs[1].addr,
					Timestamp: time.Now().Add(-time.Hour).Unix(),
					State:     ring.ACTIVE,
					Tokens:    sortTokens([]uint32{user1Group2Token + 1, user3Group1Token + 1}),
				}
			},

			expectedRules: expectedRulesMap{
				// This ruler doesn't get rules from unhealthy ruler (RF=1).
				rulerDescs[0].id: map[string]rulespb.RuleGroupList{
					user1: {user1Group1},
					user2: {user2Group1},
				},
				rulerDescs[1].id: noRules,
			},
		},

		"10 default sharding, LEAVING ruler": {
			sharding:         true,
			shardingStrategy: util.ShardingStrategyDefault,

			setupRing: func(desc *ring.Desc) {
				desc.AddIngester(rulerDescs[0].id, rulerDescs[0].addr, rulerDescs[0].zone, sortTokens([]uint32{user1Group1Token + 1, user2Group1Token + 1}), ring.LEAVING, time.Now())
				desc.AddIngester(rulerDescs[1].id, rulerDescs[1].addr, rulerDescs[1].zone, sortTokens([]uint32{user1Group2Token + 1, user3Group1Token + 1}), ring.ACTIVE, time.Now())
			},

			expectedRules: expectedRulesMap{
				// LEAVING ruler doesn't get any rules.
				rulerDescs[0].id: noRules,
				rulerDescs[1].id: allRules,
			},
		},

		"11 default sharding, JOINING ruler": {
			sharding:         true,
			shardingStrategy: util.ShardingStrategyDefault,

			setupRing: func(desc *ring.Desc) {
				desc.AddIngester(rulerDescs[0].id, rulerDescs[0].addr, rulerDescs[0].zone, sortTokens([]uint32{user1Group1Token + 1, user2Group1Token + 1}), ring.JOINING, time.Now())
				desc.AddIngester(rulerDescs[1].id, rulerDescs[1].addr, rulerDescs[1].zone, sortTokens([]uint32{user1Group2Token + 1, user3Group1Token + 1}), ring.ACTIVE, time.Now())
			},

			expectedRules: expectedRulesMap{
				// JOINING ruler has no rules yet.
				rulerDescs[0].id: noRules,
				rulerDescs[1].id: allRules,
			},
		},

		"12 shuffle sharding, single ruler": {
			sharding:         true,
			shardingStrategy: util.ShardingStrategyShuffle,

			setupRing: func(desc *ring.Desc) {
				desc.AddIngester(rulerDescs[0].id, rulerDescs[0].addr, rulerDescs[0].zone, sortTokens([]uint32{0}), ring.ACTIVE, time.Now())
			},

			expectedRules: expectedRulesMap{
				rulerDescs[0].id: allRules,
			},
		},

		"13 shuffle sharding, multiple rulerDescs, shard size 1": {
			sharding:         true,
			shardingStrategy: util.ShardingStrategyShuffle,
			shuffleShardSize: 1,

			setupRing: func(desc *ring.Desc) {
				desc.AddIngester(rulerDescs[0].id, rulerDescs[0].addr, rulerDescs[0].zone, sortTokens([]uint32{userToken(user1, 0, "") + 1, userToken(user2, 0, "") + 1, userToken(user3, 0, "") + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[1].id, rulerDescs[1].addr, rulerDescs[1].zone, sortTokens([]uint32{user1Group1Token + 1, user1Group2Token + 1, user2Group1Token + 1, user3Group1Token + 1}), ring.ACTIVE, time.Now())
			},

			expectedRules: expectedRulesMap{
				rulerDescs[0].id: allRules,
				rulerDescs[1].id: noRules,
			},
		},

		// Same test as previous one, but with shard size=2. Second ruler gets all the rules.
		"14 shuffle sharding, two rulerDescs, shard size 2": {
			sharding:         true,
			shardingStrategy: util.ShardingStrategyShuffle,
			shuffleShardSize: 2,

			setupRing: func(desc *ring.Desc) {
				// Exact same tokens setup as previous test.
				desc.AddIngester(rulerDescs[0].id, rulerDescs[0].addr, rulerDescs[0].zone, sortTokens([]uint32{userToken(user1, 0, "") + 1, userToken(user2, 0, "") + 1, userToken(user3, 0, "") + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[1].id, rulerDescs[1].addr, rulerDescs[1].zone, sortTokens([]uint32{user1Group1Token + 1, user1Group2Token + 1, user2Group1Token + 1, user3Group1Token + 1}), ring.ACTIVE, time.Now())
			},

			expectedRules: expectedRulesMap{
				rulerDescs[0].id: noRules,
				rulerDescs[1].id: allRules,
			},
		},

		"15 shuffle sharding, two rulerDescs, shard size 1, distributed users": {
			sharding:         true,
			shardingStrategy: util.ShardingStrategyShuffle,
			shuffleShardSize: 1,

			setupRing: func(desc *ring.Desc) {
				desc.AddIngester(rulerDescs[0].id, rulerDescs[0].addr, rulerDescs[0].zone, sortTokens([]uint32{userToken(user1, 0, "") + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[1].id, rulerDescs[1].addr, rulerDescs[1].zone, sortTokens([]uint32{userToken(user2, 0, "") + 1, userToken(user3, 0, "") + 1}), ring.ACTIVE, time.Now())
			},

			expectedRules: expectedRulesMap{
				rulerDescs[0].id: map[string]rulespb.RuleGroupList{
					user1: {user1Group1, user1Group2},
				},
				rulerDescs[1].id: map[string]rulespb.RuleGroupList{
					user2: {user2Group1},
					user3: {user3Group1},
				},
			},
		},
		"16 shuffle sharding, three rulerDescs, shard size 2": {
			sharding:         true,
			shardingStrategy: util.ShardingStrategyShuffle,
			shuffleShardSize: 2,

			setupRing: func(desc *ring.Desc) {
				desc.AddIngester(rulerDescs[0].id, rulerDescs[0].addr, rulerDescs[0].zone, sortTokens([]uint32{userToken(user1, 0, "") + 1, user1Group1Token + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[1].id, rulerDescs[1].addr, rulerDescs[1].zone, sortTokens([]uint32{userToken(user1, 1, "") + 1, user1Group2Token + 1, userToken(user2, 1, "") + 1, userToken(user3, 1, "") + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[2].id, rulerDescs[2].addr, rulerDescs[2].zone, sortTokens([]uint32{userToken(user2, 0, "") + 1, userToken(user3, 0, "") + 1, user2Group1Token + 1, user3Group1Token + 1}), ring.ACTIVE, time.Now())
			},

			expectedRules: expectedRulesMap{
				rulerDescs[0].id: map[string]rulespb.RuleGroupList{
					user1: {user1Group1},
				},
				rulerDescs[1].id: map[string]rulespb.RuleGroupList{
					user1: {user1Group2},
				},
				rulerDescs[2].id: map[string]rulespb.RuleGroupList{
					user2: {user2Group1},
					user3: {user3Group1},
				},
			},
		},
		"17 shuffle sharding, three rulerDescs, shard size 2, ruler2 has no users": {
			sharding:         true,
			shardingStrategy: util.ShardingStrategyShuffle,
			shuffleShardSize: 2,

			setupRing: func(desc *ring.Desc) {
				desc.AddIngester(rulerDescs[0].id, rulerDescs[0].addr, rulerDescs[0].zone, sortTokens([]uint32{userToken(user1, 0, "") + 1, userToken(user2, 1, "") + 1, user1Group1Token + 1, user1Group2Token + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[1].id, rulerDescs[1].addr, rulerDescs[1].zone, sortTokens([]uint32{userToken(user1, 1, "") + 1, userToken(user3, 1, "") + 1, user2Group1Token + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[2].id, rulerDescs[2].addr, rulerDescs[2].zone, sortTokens([]uint32{userToken(user2, 0, "") + 1, userToken(user3, 0, "") + 1, user3Group1Token + 1}), ring.ACTIVE, time.Now())
			},

			expectedRules: expectedRulesMap{
				rulerDescs[0].id: map[string]rulespb.RuleGroupList{
					user1: {user1Group1, user1Group2},
				},
				rulerDescs[1].id: noRules, // Ruler2 owns token for user2group1, but user-2 will only be handled by ruler-1 and 3.
				rulerDescs[2].id: map[string]rulespb.RuleGroupList{
					user2: {user2Group1},
					user3: {user3Group1},
				},
			},
		},

		"18 shuffle sharding, three rulerDescs, shard size 2, single enabled user": {
			sharding:         true,
			shardingStrategy: util.ShardingStrategyShuffle,
			shuffleShardSize: 2,
			enabledUsers:     []string{user1},

			setupRing: func(desc *ring.Desc) {
				desc.AddIngester(rulerDescs[0].id, rulerDescs[0].addr, rulerDescs[0].zone, sortTokens([]uint32{userToken(user1, 0, "") + 1, user1Group1Token + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[1].id, rulerDescs[1].addr, rulerDescs[1].zone, sortTokens([]uint32{userToken(user1, 1, "") + 1, user1Group2Token + 1, userToken(user2, 1, "") + 1, userToken(user3, 1, "") + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[2].id, rulerDescs[2].addr, rulerDescs[2].zone, sortTokens([]uint32{userToken(user2, 0, "") + 1, userToken(user3, 0, "") + 1, user2Group1Token + 1, user3Group1Token + 1}), ring.ACTIVE, time.Now())
			},

			expectedRules: expectedRulesMap{
				rulerDescs[0].id: map[string]rulespb.RuleGroupList{
					user1: {user1Group1},
				},
				rulerDescs[1].id: map[string]rulespb.RuleGroupList{
					user1: {user1Group2},
				},
				rulerDescs[2].id: map[string]rulespb.RuleGroupList{},
			},
		},

		"19 shuffle sharding, three rulerDescs, shard size 2, single disabled user": {
			sharding:         true,
			shardingStrategy: util.ShardingStrategyShuffle,
			shuffleShardSize: 2,
			disabledUsers:    []string{user1},

			setupRing: func(desc *ring.Desc) {
				desc.AddIngester(rulerDescs[0].id, rulerDescs[0].addr, rulerDescs[0].zone, sortTokens([]uint32{userToken(user1, 0, "") + 1, user1Group1Token + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[1].id, rulerDescs[1].addr, rulerDescs[1].zone, sortTokens([]uint32{userToken(user1, 1, "") + 1, user1Group2Token + 1, userToken(user2, 1, "") + 1, userToken(user3, 1, "") + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[2].id, rulerDescs[2].addr, rulerDescs[2].zone, sortTokens([]uint32{userToken(user2, 0, "") + 1, userToken(user3, 0, "") + 1, user2Group1Token + 1, user3Group1Token + 1}), ring.ACTIVE, time.Now())
			},

			expectedRules: expectedRulesMap{
				rulerDescs[0].id: map[string]rulespb.RuleGroupList{},
				rulerDescs[1].id: map[string]rulespb.RuleGroupList{},
				rulerDescs[2].id: map[string]rulespb.RuleGroupList{
					user2: {user2Group1},
					user3: {user3Group1},
				},
			},
		},

		"20 shuffle sharding, three rulerDescs, shard size 3, zone awareness enabled, single disabled user": {
			sharding:         true,
			shardingStrategy: util.ShardingStrategyShuffle,
			shuffleShardSize: 3,
			zoneAwareness:    true,
			disabledUsers:    []string{user3},

			setupRing: func(desc *ring.Desc) {
				desc.AddIngester(rulerDescs[0].id, rulerDescs[0].addr, rulerDescs[0].zone, sortTokens([]uint32{userToken(user1, 0, zone1) + 1, user1Group1Token + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[1].id, rulerDescs[1].addr, rulerDescs[1].zone, sortTokens([]uint32{userToken(user1, 1, zone2) + 1, user1Group2Token + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[2].id, rulerDescs[2].addr, rulerDescs[2].zone, sortTokens([]uint32{userToken(user2, 0, zone3) + 1, user2Group1Token + 1}), ring.ACTIVE, time.Now())
			},

			expectedRules: expectedRulesMap{
				rulerDescs[0].id: map[string]rulespb.RuleGroupList{
					user1: {user1Group1},
				},
				rulerDescs[1].id: map[string]rulespb.RuleGroupList{
					user1: {user1Group2},
				},
				rulerDescs[2].id: map[string]rulespb.RuleGroupList{
					user2: {user2Group1},
				},
			},
		},

		"21 shuffle sharding, three rulerDescs, shard size 3, replicationFactor 3, single enabled user": {
			sharding:          true,
			shardingStrategy:  util.ShardingStrategyShuffle,
			shuffleShardSize:  3,
			replicationFactor: 3,
			enabledUsers:      []string{user1},

			setupRing: func(desc *ring.Desc) {
				desc.AddIngester(rulerDescs[0].id, rulerDescs[0].addr, rulerDescs[0].zone, sortTokens([]uint32{userToken(user1, 0, zone1) + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[1].id, rulerDescs[1].addr, rulerDescs[1].zone, sortTokens([]uint32{userToken(user1, 1, zone2) + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[2].id, rulerDescs[2].addr, rulerDescs[2].zone, sortTokens([]uint32{userToken(user1, 2, zone3) + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[3].id, rulerDescs[3].addr, rulerDescs[3].zone, sortTokens([]uint32{userToken(user1, 3, zone1) + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[4].id, rulerDescs[4].addr, rulerDescs[4].zone, sortTokens([]uint32{userToken(user1, 4, zone2) + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[5].id, rulerDescs[5].addr, rulerDescs[5].zone, sortTokens([]uint32{userToken(user1, 5, zone3) + 1}), ring.ACTIVE, time.Now())
			},

			expectedRules: expectedRulesMap{
				rulerDescs[0].id: map[string]rulespb.RuleGroupList{
					user1: {user1Group1, user1Group2},
				},
				rulerDescs[1].id: map[string]rulespb.RuleGroupList{
					user1: {user1Group1, user1Group2},
				},
				rulerDescs[2].id: map[string]rulespb.RuleGroupList{},
				rulerDescs[3].id: map[string]rulespb.RuleGroupList{
					user1: {user1Group1, user1Group2},
				},
				rulerDescs[4].id: map[string]rulespb.RuleGroupList{},
				rulerDescs[5].id: map[string]rulespb.RuleGroupList{},
			},
		},

		"22 shuffle sharding, three rulerDescs, shard size 3, replicationFactor 3, zone awareness enabled, single enabled user": {
			sharding:          true,
			shardingStrategy:  util.ShardingStrategyShuffle,
			shuffleShardSize:  3,
			replicationFactor: 3,
			zoneAwareness:     true,
			enabledUsers:      []string{user1},

			setupRing: func(desc *ring.Desc) {
				desc.AddIngester(rulerDescs[0].id, rulerDescs[0].addr, rulerDescs[0].zone, sortTokens([]uint32{userToken(user1, 0, zone1) + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[1].id, rulerDescs[1].addr, rulerDescs[1].zone, sortTokens([]uint32{userToken(user1, 1, zone2) + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[2].id, rulerDescs[2].addr, rulerDescs[2].zone, sortTokens([]uint32{userToken(user1, 2, zone3) + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[3].id, rulerDescs[3].addr, rulerDescs[3].zone, sortTokens([]uint32{userToken(user1, 3, zone1) + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[4].id, rulerDescs[4].addr, rulerDescs[4].zone, sortTokens([]uint32{userToken(user1, 4, zone2) + 1}), ring.ACTIVE, time.Now())
				desc.AddIngester(rulerDescs[5].id, rulerDescs[5].addr, rulerDescs[5].zone, sortTokens([]uint32{userToken(user1, 5, zone3) + 1}), ring.ACTIVE, time.Now())
			},

			expectedRules: expectedRulesMap{
				rulerDescs[0].id: map[string]rulespb.RuleGroupList{
					user1: {user1Group1, user1Group2},
				},
				rulerDescs[1].id: map[string]rulespb.RuleGroupList{
					user1: {user1Group1, user1Group2},
				},
				rulerDescs[2].id: map[string]rulespb.RuleGroupList{
					user1: {user1Group1, user1Group2},
				},
				rulerDescs[3].id: map[string]rulespb.RuleGroupList{},
				rulerDescs[4].id: map[string]rulespb.RuleGroupList{},
				rulerDescs[5].id: map[string]rulespb.RuleGroupList{},
			},
		},
	}

	for name, tc := range testCases {
		t.Run(name, func(t *testing.T) {
			kvStore, closer := consul.NewInMemoryClient(ring.GetCodec(), log.NewNopLogger(), nil)
			t.Cleanup(func() { assert.NoError(t, closer.Close()) })

			rulerReplicationFactor := tc.replicationFactor
			if rulerReplicationFactor == 0 {
				rulerReplicationFactor = 1
			}

			setupRuler := func(rulerInstance rulerDesc, forceRing *ring.Ring) *Ruler {
				store := newMockRuleStore(allRules)
				cfg := Config{
					EnableSharding:   tc.sharding,
					ShardingStrategy: tc.shardingStrategy,
					Ring: RingConfig{
						InstanceID:   rulerInstance.id,
						InstanceAddr: rulerInstance.host,
						InstancePort: rulerInstance.port,
						KVStore: kv.Config{
							Mock: kvStore,
						},
						HeartbeatTimeout:     1 * time.Minute,
						ReplicationFactor:    rulerReplicationFactor,
						ZoneAwarenessEnabled: tc.zoneAwareness,
						InstanceZone:         rulerInstance.zone,
					},
					FlushCheckPeriod: 0,
					EnabledTenants:   tc.enabledUsers,
					DisabledTenants:  tc.disabledUsers,
				}

				r := buildRuler(t, cfg, nil, store, nil)
				r.limits = ruleLimits{evalDelay: 0, tenantShard: tc.shuffleShardSize}

				if forceRing != nil {
					r.ring = forceRing
				}
				return r
			}

			rulers := [len(rulerDescs)]*Ruler{}
			rulers[0] = setupRuler(rulerDescs[0], nil)

			rulerRing := rulers[0].ring

			// We start ruler's ring, but nothing else (not even lifecycler).
			if rulerRing != nil {
				require.NoError(t, services.StartAndAwaitRunning(context.Background(), rulerRing))
				t.Cleanup(rulerRing.StopAsync)
			}

			if rulerRing != nil {
				// Reuse ring from r1.
				for i := 1; i < len(rulerDescs); i++ {
					rulers[i] = setupRuler(rulerDescs[i], rulerRing)
				}
			}

			if tc.setupRing != nil {
				err := kvStore.CAS(context.Background(), ringKey, func(in interface{}) (out interface{}, retry bool, err error) {
					d, _ := in.(*ring.Desc)
					if d == nil {
						d = ring.NewDesc()
					}

					tc.setupRing(d)

					return d, true, nil
				})
				require.NoError(t, err)
				// Wait a bit to make sure ruler's ring is updated.
				time.Sleep(100 * time.Millisecond)
			}

			// Always add ruler1 to expected rulerDescs, even if there is no ring (no sharding).
			loadedRules1, err := rulers[0].listRules(context.Background())
			require.NoError(t, err)

			expected := expectedRulesMap{
				rulerDescs[0].id: loadedRules1,
			}

			addToExpected := func(id string, r *Ruler) {
				// Only expect rules from other rulerDescs when using ring, and they are present in the ring.
				if r != nil && rulerRing != nil && rulerRing.HasInstance(id) {
					loaded, err := r.listRules(context.Background())
					require.NoError(t, err)
					// Normalize nil map to empty one.
					if loaded == nil {
						loaded = map[string]rulespb.RuleGroupList{}
					}
					expected[id] = loaded
				}
			}

			for i := 1; i < len(rulerDescs); i++ {
				addToExpected(rulerDescs[i].id, rulers[i])
			}

			require.Equal(t, tc.expectedRules, expected)
		})
	}
}

// User shuffle shard token.
func userToken(user string, skip int, zone string) uint32 {
	r := rand.New(rand.NewSource(util.ShuffleShardSeed(user, zone)))

	for ; skip > 0; skip-- {
		_ = r.Uint32()
	}
	return r.Uint32()
}

func sortTokens(tokens []uint32) []uint32 {
	sort.Slice(tokens, func(i, j int) bool {
		return tokens[i] < tokens[j]
	})
	return tokens
}

func TestDeleteTenantRuleGroups(t *testing.T) {
	ruleGroups := []ruleGroupKey{
		{user: "userA", namespace: "namespace", group: "group"},
		{user: "userB", namespace: "namespace1", group: "group"},
		{user: "userB", namespace: "namespace2", group: "group"},
	}

	obj, rs := setupRuleGroupsStore(t, ruleGroups)
	require.Equal(t, 3, len(obj.Objects()))

	api, err := NewRuler(Config{}, nil, nil, log.NewNopLogger(), rs, nil)
	require.NoError(t, err)

	{
		req := &http.Request{}
		resp := httptest.NewRecorder()
		api.DeleteTenantConfiguration(resp, req)

		require.Equal(t, http.StatusUnauthorized, resp.Code)
	}

	{
		callDeleteTenantAPI(t, api, "user-with-no-rule-groups")
		require.Equal(t, 3, len(obj.Objects()))

		verifyExpectedDeletedRuleGroupsForUser(t, api, "user-with-no-rule-groups", true) // Has no rule groups
		verifyExpectedDeletedRuleGroupsForUser(t, api, "userA", false)
		verifyExpectedDeletedRuleGroupsForUser(t, api, "userB", false)
	}

	{
		callDeleteTenantAPI(t, api, "userA")
		require.Equal(t, 2, len(obj.Objects()))

		verifyExpectedDeletedRuleGroupsForUser(t, api, "user-with-no-rule-groups", true) // Has no rule groups
		verifyExpectedDeletedRuleGroupsForUser(t, api, "userA", true)                    // Just deleted.
		verifyExpectedDeletedRuleGroupsForUser(t, api, "userB", false)
	}

	// Deleting same user again works fine and reports no problems.
	{
		callDeleteTenantAPI(t, api, "userA")
		require.Equal(t, 2, len(obj.Objects()))

		verifyExpectedDeletedRuleGroupsForUser(t, api, "user-with-no-rule-groups", true) // Has no rule groups
		verifyExpectedDeletedRuleGroupsForUser(t, api, "userA", true)                    // Already deleted before.
		verifyExpectedDeletedRuleGroupsForUser(t, api, "userB", false)
	}

	{
		callDeleteTenantAPI(t, api, "userB")
		require.Equal(t, 0, len(obj.Objects()))

		verifyExpectedDeletedRuleGroupsForUser(t, api, "user-with-no-rule-groups", true) // Has no rule groups
		verifyExpectedDeletedRuleGroupsForUser(t, api, "userA", true)                    // Deleted previously
		verifyExpectedDeletedRuleGroupsForUser(t, api, "userB", true)                    // Just deleted
	}
}

func generateTokenForGroups(groups []*rulespb.RuleGroupDesc, offset uint32) []uint32 {
	var tokens []uint32

	for _, g := range groups {
		tokens = append(tokens, tokenForGroup(g)+offset)
	}

	return tokens
}

func callDeleteTenantAPI(t *testing.T, api *Ruler, userID string) {
	ctx := user.InjectOrgID(context.Background(), userID)

	req := &http.Request{}
	resp := httptest.NewRecorder()
	api.DeleteTenantConfiguration(resp, req.WithContext(ctx))

	require.Equal(t, http.StatusOK, resp.Code)
}

func verifyExpectedDeletedRuleGroupsForUser(t *testing.T, r *Ruler, userID string, expectedDeleted bool) {
	list, err := r.store.ListRuleGroupsForUserAndNamespace(context.Background(), userID, "")
	require.NoError(t, err)

	if expectedDeleted {
		require.Equal(t, 0, len(list))
	} else {
		require.NotEqual(t, 0, len(list))
	}
}

func setupRuleGroupsStore(t *testing.T, ruleGroups []ruleGroupKey) (*objstore.InMemBucket, rulestore.RuleStore) {
	bucketClient := objstore.NewInMemBucket()
	rs := bucketclient.NewBucketRuleStore(bucketClient, nil, log.NewNopLogger())

	// "upload" rule groups
	for _, key := range ruleGroups {
		desc := rulespb.ToProto(key.user, key.namespace, rulefmt.RuleGroup{Name: key.group})
		require.NoError(t, rs.SetRuleGroup(context.Background(), key.user, key.namespace, desc))
	}

	return bucketClient, rs
}

type ruleGroupKey struct {
	user, namespace, group string
}

func TestRuler_ListAllRules(t *testing.T) {
	store := newMockRuleStore(mockRules)
	cfg := defaultRulerConfig(t)

	r := newTestRuler(t, cfg, store, nil)
	defer services.StopAndAwaitTerminated(context.Background(), r) //nolint:errcheck

	router := mux.NewRouter()
	router.Path("/ruler/rule_groups").Methods(http.MethodGet).HandlerFunc(r.ListAllRules)

	req := requestFor(t, http.MethodGet, "https://localhost:8080/ruler/rule_groups", nil, "")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)

	// Check status code and header
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/yaml", resp.Header.Get("Content-Type"))

	gs := make(map[string]map[string][]rulefmt.RuleGroup) // user:namespace:[]rulefmt.RuleGroup
	for userID := range mockRules {
		gs[userID] = mockRules[userID].Formatted()
	}

	// check for unnecessary fields
	unnecessaryFields := []string{"kind", "style", "tag", "value", "anchor", "alias", "content", "headcomment", "linecomment", "footcomment", "line", "column"}
	for _, word := range unnecessaryFields {
		require.NotContains(t, string(body), word)
	}

	expectedResponse, err := yaml.Marshal(gs)
	require.NoError(t, err)
	require.YAMLEq(t, string(expectedResponse), string(body))
}

type senderFunc func(alerts ...*notifier.Alert)

func (s senderFunc) Send(alerts ...*notifier.Alert) {
	s(alerts...)
}

func TestSendAlerts(t *testing.T) {
	testCases := []struct {
		in  []*promRules.Alert
		exp []*notifier.Alert
	}{
		{
			in: []*promRules.Alert{
				{
					Labels:      []labels.Label{{Name: "l1", Value: "v1"}},
					Annotations: []labels.Label{{Name: "a2", Value: "v2"}},
					ActiveAt:    time.Unix(1, 0),
					FiredAt:     time.Unix(2, 0),
					ValidUntil:  time.Unix(3, 0),
				},
			},
			exp: []*notifier.Alert{
				{
					Labels:       []labels.Label{{Name: "l1", Value: "v1"}},
					Annotations:  []labels.Label{{Name: "a2", Value: "v2"}},
					StartsAt:     time.Unix(2, 0),
					EndsAt:       time.Unix(3, 0),
					GeneratorURL: "http://localhost:9090/graph?g0.expr=up&g0.tab=1",
				},
			},
		},
		{
			in: []*promRules.Alert{
				{
					Labels:      []labels.Label{{Name: "l1", Value: "v1"}},
					Annotations: []labels.Label{{Name: "a2", Value: "v2"}},
					ActiveAt:    time.Unix(1, 0),
					FiredAt:     time.Unix(2, 0),
					ResolvedAt:  time.Unix(4, 0),
				},
			},
			exp: []*notifier.Alert{
				{
					Labels:       []labels.Label{{Name: "l1", Value: "v1"}},
					Annotations:  []labels.Label{{Name: "a2", Value: "v2"}},
					StartsAt:     time.Unix(2, 0),
					EndsAt:       time.Unix(4, 0),
					GeneratorURL: "http://localhost:9090/graph?g0.expr=up&g0.tab=1",
				},
			},
		},
		{
			in: []*promRules.Alert{},
		},
	}

	for i, tc := range testCases {
		tc := tc
		t.Run(fmt.Sprintf("%d", i), func(t *testing.T) {
			senderFunc := senderFunc(func(alerts ...*notifier.Alert) {
				if len(tc.in) == 0 {
					t.Fatalf("sender called with 0 alert")
				}
				require.Equal(t, tc.exp, alerts)
			})
			SendAlerts(senderFunc, "http://localhost:9090")(context.TODO(), "up", tc.in...)
		})
	}
}

// Tests for whether the Ruler is able to recover ALERTS_FOR_STATE state
func TestRecoverAlertsPostOutage(t *testing.T) {
	// Test Setup
	// alert FOR 30m, already ran for 10m, outage down at 15m prior to now(), outage tolerance set to 1hr
	// EXPECTATION: for state for alert restores to 10m+(now-15m)

	// FIRST set up 1 Alert rule with 30m FOR duration
	alertForDuration, _ := time.ParseDuration("30m")
	mockRules := map[string]rulespb.RuleGroupList{
		"user1": {
			&rulespb.RuleGroupDesc{
				Name:      "group1",
				Namespace: "namespace1",
				User:      "user1",
				Rules: []*rulespb.RuleDesc{
					{
						Alert: "UP_ALERT",
						Expr:  "1", // always fire for this test
						For:   alertForDuration,
					},
				},
				Interval: interval,
			},
		},
	}

	// NEXT, set up ruler config with outage tolerance = 1hr
	store := newMockRuleStore(mockRules)
	rulerCfg := defaultRulerConfig(t)
	rulerCfg.OutageTolerance, _ = time.ParseDuration("1h")

	// NEXT, set up mock distributor containing sample,
	// metric: ALERTS_FOR_STATE{alertname="UP_ALERT"}, ts: time.now()-15m, value: time.now()-25m
	currentTime := time.Now().UTC()
	downAtTime := currentTime.Add(time.Minute * -15)
	downAtTimeMs := downAtTime.UnixNano() / int64(time.Millisecond)
	downAtActiveAtTime := currentTime.Add(time.Minute * -25)
	downAtActiveSec := downAtActiveAtTime.Unix()
	d := &querier.MockDistributor{}
	d.On("Query", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Return(
		model.Matrix{
			&model.SampleStream{
				Metric: model.Metric{
					labels.MetricName: "ALERTS_FOR_STATE",
					// user1's only alert rule
					labels.AlertName: model.LabelValue(mockRules["user1"][0].GetRules()[0].Alert),
				},
				Values: []model.SamplePair{{Timestamp: model.Time(downAtTimeMs), Value: model.SampleValue(downAtActiveSec)}},
			},
		},
		nil)
	d.On("MetricsForLabelMatchers", mock.Anything, mock.Anything, mock.Anything, mock.Anything).Panic("This should not be called for the ruler use-cases.")
	querierConfig := querier.DefaultQuerierConfig()
	querierConfig.IngesterStreaming = false

	// set up an empty store
	queryables := []querier.QueryableWithFilter{
		querier.UseAlwaysQueryable(newEmptyQueryable()),
	}

	// create a ruler but don't start it. instead, we'll evaluate the rule groups manually.
	r := buildRuler(t, rulerCfg, &querier.TestConfig{Cfg: querierConfig, Distributor: d, Stores: queryables}, store, nil)
	r.syncRules(context.Background(), rulerSyncReasonInitial)

	// assert initial state of rule group
	ruleGroup := r.manager.GetRules("user1")[0]
	require.Equal(t, time.Time{}, ruleGroup.GetLastEvaluation())
	require.Equal(t, "group1", ruleGroup.Name())
	require.Equal(t, 1, len(ruleGroup.Rules()))

	// assert initial state of rule within rule group
	alertRule := ruleGroup.Rules()[0]
	require.Equal(t, time.Time{}, alertRule.GetEvaluationTimestamp())
	require.Equal(t, "UP_ALERT", alertRule.Name())
	require.Equal(t, promRules.HealthUnknown, alertRule.Health())

	// NEXT, evaluate the rule group the first time and assert
	ctx := user.InjectOrgID(context.Background(), "user1")
	ruleGroup.Eval(ctx, currentTime)

	// since the eval is done at the current timestamp, the activeAt timestamp of alert should equal current timestamp
	require.Equal(t, "UP_ALERT", alertRule.Name())
	require.Equal(t, promRules.HealthGood, alertRule.Health())

	activeMapRaw := reflect.ValueOf(alertRule).Elem().FieldByName("active")
	activeMapKeys := activeMapRaw.MapKeys()
	require.True(t, len(activeMapKeys) == 1)

	activeAlertRuleRaw := activeMapRaw.MapIndex(activeMapKeys[0]).Elem()
	activeAtTimeRaw := activeAlertRuleRaw.FieldByName("ActiveAt")

	require.Equal(t, promRules.StatePending, promRules.AlertState(activeAlertRuleRaw.FieldByName("State").Int()))
	require.Equal(t, reflect.NewAt(activeAtTimeRaw.Type(), unsafe.Pointer(activeAtTimeRaw.UnsafeAddr())).Elem().Interface().(time.Time), currentTime)

	// NEXT, restore the FOR state and assert
	ruleGroup.RestoreForState(currentTime)

	require.Equal(t, "UP_ALERT", alertRule.Name())
	require.Equal(t, promRules.HealthGood, alertRule.Health())
	require.Equal(t, promRules.StatePending, promRules.AlertState(activeAlertRuleRaw.FieldByName("State").Int()))
	require.Equal(t, reflect.NewAt(activeAtTimeRaw.Type(), unsafe.Pointer(activeAtTimeRaw.UnsafeAddr())).Elem().Interface().(time.Time), downAtActiveAtTime.Add(currentTime.Sub(downAtTime)))

	// NEXT, 20 minutes is expected to be left, eval timestamp at currentTimestamp +20m
	currentTime = currentTime.Add(time.Minute * 20)
	ruleGroup.Eval(ctx, currentTime)

	// assert alert state after alert is firing
	firedAtRaw := activeAlertRuleRaw.FieldByName("FiredAt")
	firedAtTime := reflect.NewAt(firedAtRaw.Type(), unsafe.Pointer(firedAtRaw.UnsafeAddr())).Elem().Interface().(time.Time)
	require.Equal(t, firedAtTime, currentTime)

	require.Equal(t, promRules.StateFiring, promRules.AlertState(activeAlertRuleRaw.FieldByName("State").Int()))
}
