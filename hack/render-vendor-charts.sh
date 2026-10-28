#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Copyright 2026 CollectorCtrl
# Regenerates the vendor-chart fixtures used by
# operator/controllers/vendorcharts_test.go. Chart versions are pinned; after
# bumping one, rerun this script and review every changed expectation by hand.
set -euo pipefail
out="$(cd "$(dirname "$0")/.." && pwd)/operator/controllers/testdata/vendorcharts"
mkdir -p "$out"

helm repo add otel https://open-telemetry.github.io/opentelemetry-helm-charts >/dev/null
helm repo add splunk https://signalfx.github.io/splunk-otel-collector-chart >/dev/null
helm repo add coralogix https://cgx.jfrog.io/artifactory/coralogix-charts-virtual >/dev/null
helm repo add aws-otel https://aws-observability.github.io/aws-otel-helm-charts >/dev/null
helm repo update >/dev/null

helm template up-agent otel/opentelemetry-collector --version 0.176.0 -n observability \
  --set mode=daemonset --set image.repository=otel/opentelemetry-collector-k8s \
  --set presets.kubeletMetrics.enabled=true --set presets.logsCollection.enabled=true \
  --set presets.kubernetesAttributes.enabled=true > "$out/upstream-agent.yaml"
helm template up-gateway otel/opentelemetry-collector --version 0.176.0 -n observability \
  --set mode=deployment --set image.repository=otel/opentelemetry-collector-k8s > "$out/upstream-gateway.yaml"
helm template up-cluster otel/opentelemetry-collector --version 0.176.0 -n observability \
  --set mode=deployment --set replicaCount=1 --set image.repository=otel/opentelemetry-collector-k8s \
  --set presets.clusterMetrics.enabled=true --set presets.kubernetesEvents.enabled=true > "$out/upstream-cluster.yaml"
helm template splunk splunk/splunk-otel-collector --version 0.162.1 -n splunk \
  --set clusterName=prod --set splunkObservability.realm=us0 \
  --set splunkObservability.accessToken=dummy --set gateway.enabled=true > "$out/splunk.yaml"
helm template cx coralogix/otel-integration --version 0.0.360 -n observability \
  --set global.domain=eu2.coralogix.com --set global.clusterName=prod > "$out/coralogix.yaml"
helm template adot aws-otel/adot-exporter-for-eks-on-ec2 --version 0.22.0 -n amazon-metrics \
  --set clusterName=prod --set awsRegion=eu-west-1 > "$out/adot.yaml"

# Not covered on purpose:
# - opentelemetry-kube-stack and Elastic EDOT render OpenTelemetryCollector
#   resources, not workloads; the upstream operator creates the workloads later.
# - grafana/k8s-monitoring runs Alloy, whose config is not otelcol YAML; such
#   collectors are listed with role "unknown".
echo "fixtures written to $out"
