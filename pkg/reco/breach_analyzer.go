package reco

import (
	"context"
	"github.com/flipkart-incubator/ottoscalr/api/v1alpha1"
	"github.com/flipkart-incubator/ottoscalr/pkg/metrics"
	"github.com/flipkart-incubator/ottoscalr/pkg/policy"
	"github.com/flipkart-incubator/ottoscalr/pkg/trigger"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	p8smetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	breachGauge = promauto.NewGaugeVec(
		prometheus.GaugeOpts{Name: "breachanalyzer_breached",
			Help: "Number of breaches counter"}, []string{"namespace", "policyreco", "workloadKind", "workload"},
	)
)

func init() {
	p8smetrics.Registry.MustRegister(breachGauge)
}

type BreachAnalyzer struct {
	store    policy.Store
	scraper  metrics.Scraper
	breachFn func(ctx context.Context, start, end time.Time, workloadType string,
		workload types.NamespacedName,
		metricScraper metrics.Scraper,
		cpuRedLine float64,
		metricStep time.Duration) (bool, error)
	client     client.Client
	cpuRedline float64
	metricStep time.Duration
}

func NewBreachAnalyzer(k8sClient client.Client, scraper metrics.Scraper, cpuRedline float64, metricStep time.Duration) (*BreachAnalyzer, error) {
	return &BreachAnalyzer{
		store:      policy.NewPolicyStore(k8sClient),
		scraper:    scraper,
		breachFn:   trigger.HasBreached,
		client:     k8sClient,
		cpuRedline: cpuRedline,
		metricStep: metricStep,
	}, nil
}

func (pi *BreachAnalyzer) NextPolicy(ctx context.Context, wm WorkloadMeta) (*Policy, error) {
	logger := log.FromContext(ctx)
	currentPolicyReco := &v1alpha1.PolicyRecommendation{}
	if err := pi.client.Get(ctx, types.NamespacedName{Name: wm.Name, Namespace: wm.Namespace}, currentPolicyReco); err != nil {
		logger.V(0).Error(err, "Error while fetching policy reco", "workload", wm)
		return nil, err
	}

	if len(currentPolicyReco.Spec.Policy) == 0 {
		logger.V(0).Info("Empty policy in policy reco. Falling back to no-op.")
		return nil, nil
	}

	if currentPolicyReco.Spec.GeneratedAt == nil || currentPolicyReco.Spec.GeneratedAt.IsZero() {
		logger.V(0).Info("Policy reco has nil generatedAt field. Falling back to no-op.")
		return nil, nil
	}

	end := time.Now()
	start := currentPolicyReco.Spec.GeneratedAt.Time
	breached, err := pi.breachFn(ctx, start, end, wm.Kind, types.NamespacedName{
		Namespace: wm.Namespace,
		Name:      wm.Name,
	}, pi.scraper, pi.cpuRedline, pi.metricStep)
	if err != nil {
		logger.V(0).Error(err, "Error running breach detector")
		return nil, err
	}
	if breached {
		currentPolicyReco := &v1alpha1.PolicyRecommendation{}
		if err2 := pi.client.Get(ctx, types.NamespacedName{Name: wm.Name, Namespace: wm.Namespace}, currentPolicyReco); client.IgnoreNotFound(err2) != nil {
			logger.V(0).Error(err2, "Error while fetching policy reco", "workload", wm)
			return nil, err2
		}
		saferPolicy, err3 := pi.store.GetPreviousPolicyByName(currentPolicyReco.Spec.Policy)
		if err3 != nil {
			if policy.IsSafestPolicy(err3) {
				logger.V(0).Error(err3, "No safer policy found. Falling back to no-op.")
				return nil, nil
			}
			logger.V(0).Error(err3, "Error fetching the previous policy.")
			return nil, err
		}
		breachGauge.WithLabelValues(wm.Namespace, currentPolicyReco.Name, wm.Kind, wm.Name).Set(1)
		return PolicyFromCR(saferPolicy), nil
	}
	breachGauge.WithLabelValues(wm.Namespace, currentPolicyReco.Name, wm.Kind, wm.Name).Set(0)
	return nil, nil
}

func (pi *BreachAnalyzer) GetName() string {
	return "BreachAnalyzer"
}
