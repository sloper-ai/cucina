# SPDX-License-Identifier: FSL-1.1-ALv2
# Generates files/dashboards/cucina-overview.json (R-OBS-2): `jq -n -c -f overview-dashboard.jq`.
# Metric names: docs/contracts.md §6 and files/rules/cucina.yaml.
def ds: {type: "prometheus", uid: "${DS_PROMETHEUS}"};
def target(expr; legend): {datasource: ds, expr: expr, legendFormat: legend, refId: "A"};
def targets(ts): [ts | to_entries[] | .value + {refId: ([65 + .key] | implode)}];
def panel(id; type; title; x; y; w; h; unit; ts):
  {id: id, type: type, title: title, datasource: ds,
   gridPos: {x: x, y: y, w: w, h: h},
   fieldConfig: {defaults: {unit: unit}, overrides: []},
   options: (if type == "stat" then {reduceOptions: {calcs: ["lastNotNull"]}, graphMode: "area"} else {legend: {displayMode: "list", placement: "bottom"}, tooltip: {mode: "multi"}} end),
   targets: targets(ts)};
def row(id; title; y): {id: id, type: "row", title: title, collapsed: false, gridPos: {x: 0, y: y, w: 24, h: 1}, panels: []};
{
  uid: "cucina-overview",
  title: "Cucina / Overview",
  tags: ["cucina"],
  timezone: "utc",
  schemaVersion: 39,
  refresh: "30s",
  time: {from: "now-6h", to: "now"},
  editable: true,
  templating: {list: [
    {name: "DS_PROMETHEUS", label: "Datasource", type: "datasource", query: "prometheus", current: {}, hide: 0},
    {name: "namespace", label: "Namespace", type: "query", datasource: ds,
     query: "label_values(cucina_pool_desired, namespace)", refresh: 2, includeAll: true, multi: false, current: {}}
  ]},
  panels: [
    row(1; "Now"; 0),
    panel(2; "stat"; "Queued actions"; 0; 1; 4; 4; "short"; [target("sum(cucina_queue_queued{namespace=~\"$namespace\"})"; "queued")]),
    panel(3; "stat"; "Workers registered"; 4; 1; 4; 4; "short"; [target("sum(cucina_pool_vms{namespace=~\"$namespace\", state=~\"busy|idle\"})"; "VMs")]),
    panel(4; "stat"; "AC hit ratio (1h)"; 8; 1; 4; 4; "percentunit"; [target("cucina:ac_hit_ratio:rate1h{namespace=~\"$namespace\"}"; "hit ratio")]),
    panel(5; "stat"; "CAS retention"; 12; 1; 4; 4; "s"; [target("cucina:cas_retention_seconds{namespace=~\"$namespace\"}"; "retention")]),
    panel(6; "stat"; "Spend, last 24 h"; 16; 1; 4; 4; "currencyUSD"; [target("sum(increase(cucina_cost_usd_total{namespace=~\"$namespace\"}[24h]))"; "USD")]),
    panel(7; "stat"; "Standing cost / month"; 20; 1; 4; 4; "currencyUSD"; [target("sum(cucina_standing_cost_usd_per_month{namespace=~\"$namespace\"})"; "USD/month")]),
    row(8; "Queues"; 5),
    panel(9; "timeseries"; "Queue depth per platform"; 0; 6; 8; 8; "short"; [target("sum by (platform, size_class) (cucina_queue_queued{namespace=~\"$namespace\"})"; "{{platform}} [{{size_class}}]")]),
    panel(10; "timeseries"; "Oldest queued action per platform"; 8; 6; 8; 8; "s"; [target("max by (platform) (cucina_queue_oldest_seconds{namespace=~\"$namespace\"})"; "{{platform}}")]),
    panel(11; "timeseries"; "Scheduler queue time p95"; 16; 6; 8; 8; "s"; [target("histogram_quantile(0.95, sum by (le, instance_name_prefix) (rate(buildbarn_builder_in_memory_build_queue_tasks_queued_duration_seconds_bucket{namespace=~\"$namespace\", cucina_component=\"scheduler\"}[5m])))"; "{{instance_name_prefix}}")]),
    row(12; "Workers"; 14),
    panel(13; "timeseries"; "Workers by state"; 0; 15; 8; 8; "short"; [target("sum by (state) (cucina_pool_vms{namespace=~\"$namespace\"})"; "{{state}}")]),
    panel(14; "timeseries"; "Desired vs running per pool"; 8; 15; 8; 8; "short"; [target("max by (pool) (cucina_pool_desired{namespace=~\"$namespace\"})"; "{{pool}} desired"), target("sum by (pool) (cucina_pool_vms{namespace=~\"$namespace\", state=~\"launching|busy|idle|draining\"})"; "{{pool}} running")]),
    panel(15; "timeseries"; "Cold start to first action (p50 / p95)"; 16; 15; 8; 8; "s"; [target("histogram_quantile(0.5, sum by (le, pool) (rate(cucina_vm_start_seconds_bucket{namespace=~\"$namespace\", phase=\"to_first_action\"}[1h])))"; "{{pool}} p50"), target("histogram_quantile(0.95, sum by (le, pool) (rate(cucina_vm_start_seconds_bucket{namespace=~\"$namespace\", phase=\"to_first_action\"}[1h])))"; "{{pool}} p95")]),
    row(16; "Cache tiers"; 23),
    panel(17; "timeseries"; "Hit ratio per tier"; 0; 24; 12; 8; "percentunit"; [
      target("cucina:ac_hit_ratio:rate5m{namespace=~\"$namespace\"}"; "L3 action cache"),
      target("sum(rate(cucina_hostd_l2_requests_total{namespace=~\"$namespace\", result=\"hit\"}[5m])) / sum(rate(cucina_hostd_l2_requests_total{namespace=~\"$namespace\"}[5m]))"; "L2 Mac hosts"),
      target("sum(rate(buildbarn_blobstore_blob_access_operations_duration_seconds_count{namespace=~\"$namespace\", cucina_component=\"worker\", storage_type=\"cas\", backend_type=~\"local_.*\", operation=\"Get\", grpc_code=\"OK\"}[5m])) / sum(rate(buildbarn_blobstore_blob_access_operations_duration_seconds_count{namespace=~\"$namespace\", cucina_component=\"worker\", storage_type=\"cas\", backend_type=~\"local_.*\", operation=\"Get\", grpc_code=~\"OK|NotFound\"}[5m]))"; "L1 EC2 workers")]),
    panel(18; "timeseries"; "Bytes per tier"; 12; 24; 12; 8; "Bps"; [
      target("cucina:frontend_cas_read_bytes:rate5m{namespace=~\"$namespace\"}"; "L3 reads (frontend)"),
      target("cucina:hostd_wan_received_bytes:rate5m{namespace=~\"$namespace\"}"; "WAN to Mac sites"),
      target("sum(rate(buildbarn_blobstore_blob_access_operations_blob_size_bytes_sum{namespace=~\"$namespace\", cucina_component=\"worker\", storage_type=\"cas\", backend_type=\"read_caching\", operation=\"Get\"}[5m]))"; "L1 reads (EC2 workers)")]),
    row(19; "Storage"; 32),
    panel(20; "timeseries"; "Retention per store"; 0; 33; 12; 8; "s"; [target("cucina:storage_retention_seconds{namespace=~\"$namespace\"}"; "{{storage_type}}")]),
    panel(21; "timeseries"; "Storage volume usage"; 12; 33; 12; 8; "percentunit"; [target("1 - kubelet_volume_stats_available_bytes{namespace=~\"$namespace\", persistentvolumeclaim=~\".*-storage-[0-9]+\"} / kubelet_volume_stats_capacity_bytes{namespace=~\"$namespace\", persistentvolumeclaim=~\".*-storage-[0-9]+\"}"; "{{persistentvolumeclaim}}")]),
    row(22; "Spend"; 41),
    panel(23; "timeseries"; "Spend rate by pool and category"; 0; 42; 12; 8; "currencyUSD"; [target("sum by (pool, category) (rate(cucina_cost_usd_total{namespace=~\"$namespace\"}[1h])) * 3600"; "{{pool}} {{category}} (USD/h)")]),
    panel(24; "timeseries"; "Running instances by pool and type"; 12; 42; 12; 8; "short"; [target("sum by (pool, type) (rate(cucina_instance_seconds_total{namespace=~\"$namespace\"}[5m]))"; "{{pool}} {{type}}")])
  ]
}
