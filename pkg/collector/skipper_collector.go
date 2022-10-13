package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	rgv1 "github.com/szuecs/routegroup-client/apis/zalando.org/v1"
	rginterface "github.com/szuecs/routegroup-client/client/clientset/versioned"
	autoscalingv2 "k8s.io/api/autoscaling/v2beta2"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	SkipperMetricType         = "skipper"
	rpsQuery                  = `scalar(sum(rate(skipper_serve_host_duration_seconds_count{host=~"%s"}[1m])) * %.4f)`
	rpsMetricName             = "requests-per-second"
	rpsMetricBackendSeparator = ","
)

var (
	errBackendNameMissing = errors.New("backend name must be specified for requests-per-second when traffic switching is used")
)

// SkipperCollectorPlugin is a collector plugin for initializing metrics
// collectors for getting skipper ingress metrics.
type SkipperCollectorPlugin struct {
	client             kubernetes.Interface
	rgClient           rginterface.Interface
	plugin             CollectorPlugin
	backendAnnotations []string
}

// NewSkipperCollectorPlugin initializes a new SkipperCollectorPlugin.
func NewSkipperCollectorPlugin(client kubernetes.Interface, rgClient rginterface.Interface, prometheusPlugin *PrometheusCollectorPlugin, backendAnnotations []string) (*SkipperCollectorPlugin, error) {
	return &SkipperCollectorPlugin{
		client:             client,
		rgClient:           rgClient,
		plugin:             prometheusPlugin,
		backendAnnotations: backendAnnotations,
	}, nil
}

// NewCollector initializes a new skipper collector from the specified HPA.
func (c *SkipperCollectorPlugin) NewCollector(hpa *autoscalingv2.HorizontalPodAutoscaler, config *MetricConfig, interval time.Duration) (Collector, error) {
	if config.Type == autoscalingv2.ExternalMetricSourceType {
		if config.Metric.Selector == nil {
			return nil, fmt.Errorf("selector is not specified")
		}
		metricName, ok := config.Metric.Selector.MatchLabels["metric"]
		if !ok {
			return nil, fmt.Errorf("metric name is not specified")
		}
		if metricName != rpsMetricName {
			return nil, fmt.Errorf("metric %s is not supported", metricName)
		}
		if _, ok := config.Metric.Selector.MatchLabels["host"]; !ok {
			return nil, fmt.Errorf("host is not specified")
		}
		return NewSkipperCollector(nil, nil, c.plugin, hpa, config, interval, nil, "")
	}

	if strings.HasPrefix(config.Metric.Name, rpsMetricName) {
		backend, ok := config.Config["backend"]
		if !ok {
			// TODO: remove the deprecated way of specifying
			// optional backend at a later point in time.
			if len(config.Metric.Name) > len(rpsMetricName) {
				metricNameParts := strings.Split(config.Metric.Name, rpsMetricBackendSeparator)
				if len(metricNameParts) == 2 {
					backend = metricNameParts[1]
				}
			}
		}
		return NewSkipperCollector(c.client, c.rgClient, c.plugin, hpa, config, interval, c.backendAnnotations, backend)
	}
	return nil, fmt.Errorf("metric '%s' not supported", config.Metric.Name)
}

// SkipperCollector is a metrics collector for getting skipper ingress metrics.
// It depends on the prometheus collector for getting the metrics.
type SkipperCollector struct {
	client             kubernetes.Interface
	rgClient           rginterface.Interface
	hpa                *autoscalingv2.HorizontalPodAutoscaler
	interval           time.Duration
	plugin             CollectorPlugin
	config             MetricConfig
	backend            string
	backendAnnotations []string
}

// NewSkipperCollector initializes a new SkipperCollector.
func NewSkipperCollector(client kubernetes.Interface, rgClient rginterface.Interface, plugin CollectorPlugin, hpa *autoscalingv2.HorizontalPodAutoscaler, config *MetricConfig, interval time.Duration, backendAnnotations []string, backend string) (*SkipperCollector, error) {
	return &SkipperCollector{
		client:             client,
		rgClient:           rgClient,
		hpa:                hpa,
		interval:           interval,
		plugin:             plugin,
		config:             *config,
		backend:            backend,
		backendAnnotations: backendAnnotations,
	}, nil
}

func getAnnotationWeight(backendWeights string, backend string) (float64, error) {
	var weightsMap map[string]float64
	err := json.Unmarshal([]byte(backendWeights), &weightsMap)
	if err != nil {
		return 0, err
	}
	if weight, ok := weightsMap[backend]; ok {
		return float64(weight) / 100, nil
	}
	return 0, nil
}

func getIngressWeight(ingressAnnotations map[string]string, backendAnnotations []string, backend string) (float64, error) {
	maxWeight := 0.0
	annotationsPresent := false

	for _, anno := range backendAnnotations {
		if weightsMap, ok := ingressAnnotations[anno]; ok {
			annotationsPresent = true
			weight, err := getAnnotationWeight(weightsMap, backend)
			if err != nil {
				return 0.0, err
			}
			maxWeight = math.Max(maxWeight, weight)
		}
	}

	// Fallback for ingresses that don't use traffic switching
	if !annotationsPresent {
		return 1.0, nil
	}

	// Require backend name here
	if backend != "" {
		return maxWeight, nil
	}

	return 0.0, errBackendNameMissing
}

func getRouteGroupWeight(backends []rgv1.RouteGroupBackendReference, backendName string) (float64, error) {
	if len(backends) <= 1 {
		return 1.0, nil
	}

	if backendName == "" {
		return 0.0, errBackendNameMissing
	}

	for _, backend := range backends {
		if backend.BackendName == backendName {
			return float64(backend.Weight) / 100.0, nil
		}
	}

	return 0.0, nil
}

// getCollector returns a collector for getting the metrics.
func (c *SkipperCollector) getCollector(ctx context.Context) (Collector, error) {
	var escapedHostnames []string
	var backendWeight float64
	config := c.config

	addHost := func(host string) {
		escapedHostnames = append(escapedHostnames, regexp.QuoteMeta(strings.Replace(host, ".", "_", -1)))
	}

	if config.Type == autoscalingv2.ExternalMetricSourceType {
		backendWeight = 1.0 // backend weight is not supported
		addHost(config.Metric.Selector.MatchLabels["host"])
	} else {
		objectReference := config.ObjectReference

		switch objectReference.Kind {
		case "Ingress":
			ingress, err := c.client.NetworkingV1().Ingresses(objectReference.Namespace).Get(ctx, objectReference.Name, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}

			backendWeight, err = getIngressWeight(ingress.Annotations, c.backendAnnotations, c.backend)
			if err != nil {
				return nil, err
			}

			for _, rule := range ingress.Spec.Rules {
				addHost(rule.Host)
			}
		case "RouteGroup":
			routegroup, err := c.rgClient.ZalandoV1().RouteGroups(objectReference.Namespace).Get(ctx, objectReference.Name, metav1.GetOptions{})
			if err != nil {
				return nil, err
			}

			backendWeight, err = getRouteGroupWeight(routegroup.Spec.DefaultBackends, c.backend)
			if err != nil {
				return nil, err
			}

			for _, host := range routegroup.Spec.Hosts {
				addHost(host)
			}
		default:
			return nil, fmt.Errorf("unknown skipper resource kind %s for resource %s/%s", objectReference.Kind, objectReference.Namespace, objectReference.Name)
		}

		if len(escapedHostnames) == 0 {
			return nil, fmt.Errorf("no hosts defined on %s %s/%s, unable to create collector", objectReference.Kind, objectReference.Namespace, objectReference.Name)
		}
	}

	config.Config = map[string]string{
		"query": fmt.Sprintf(rpsQuery, strings.Join(escapedHostnames, "|"), backendWeight),
	}

	config.PerReplica = false // per replica is handled outside of the prometheus collector
	collector, err := c.plugin.NewCollector(c.hpa, &config, c.interval)
	if err != nil {
		return nil, err
	}

	return collector, nil
}

// GetMetrics gets skipper metrics from prometheus.
func (c *SkipperCollector) GetMetrics() ([]CollectedMetric, error) {
	collector, err := c.getCollector(context.TODO())
	if err != nil {
		return nil, err
	}

	values, err := collector.GetMetrics()
	if err != nil {
		return nil, err
	}

	if len(values) != 1 {
		return nil, fmt.Errorf("expected to only get one metric value, got %d", len(values))
	}

	value := values[0]

	// For Kubernetes <v1.14 we have to fall back to manual average
	if c.config.MetricSpec.Object != nil && c.config.MetricSpec.Object.Target.AverageValue == nil {
		// get current replicas for the targeted scale object. This is used to
		// calculate an average metric instead of total.
		// targetAverageValue will be available in Kubernetes v1.12
		// https://github.com/kubernetes/kubernetes/pull/64097
		replicas, err := targetRefReplicas(c.client, c.hpa)
		if err != nil {
			return nil, err
		}

		if replicas < 1 {
			return nil, fmt.Errorf("unable to get average value for %d replicas", replicas)
		}

		avgValue := float64(value.Custom.Value.MilliValue()) / float64(replicas)
		value.Custom.Value = *resource.NewMilliQuantity(int64(avgValue), resource.DecimalSI)
	}

	return []CollectedMetric{value}, nil
}

// Interval returns the interval at which the collector should run.
func (c *SkipperCollector) Interval() time.Duration {
	return c.interval
}

func targetRefReplicas(client kubernetes.Interface, hpa *autoscalingv2.HorizontalPodAutoscaler) (int32, error) {
	var replicas int32
	switch hpa.Spec.ScaleTargetRef.Kind {
	case "Deployment":
		deployment, err := client.AppsV1().Deployments(hpa.Namespace).Get(context.TODO(), hpa.Spec.ScaleTargetRef.Name, metav1.GetOptions{})
		if err != nil {
			return 0, err
		}
		replicas = deployment.Status.Replicas
	case "StatefulSet":
		sts, err := client.AppsV1().StatefulSets(hpa.Namespace).Get(context.TODO(), hpa.Spec.ScaleTargetRef.Name, metav1.GetOptions{})
		if err != nil {
			return 0, err
		}
		replicas = sts.Status.Replicas
	}

	return replicas, nil
}
