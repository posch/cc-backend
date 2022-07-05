package metricdata

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ClusterCockpit/cc-backend/internal/config"
	"github.com/ClusterCockpit/cc-backend/pkg/log"
	"github.com/ClusterCockpit/cc-backend/pkg/lrucache"
	"github.com/ClusterCockpit/cc-backend/pkg/schema"
)

type MetricDataRepository interface {
	// Initialize this MetricDataRepository. One instance of
	// this interface will only ever be responsible for one cluster.
	Init(rawConfig json.RawMessage) error

	// Return the JobData for the given job, only with the requested metrics.
	LoadData(job *schema.Job, metrics []string, scopes []schema.MetricScope, ctx context.Context) (schema.JobData, error)

	// Return a map of metrics to a map of nodes to the metric statistics of the job. node scope assumed for now.
	LoadStats(job *schema.Job, metrics []string, ctx context.Context) (map[string]map[string]schema.MetricStatistics, error)

	// Return a map of hosts to a map of metrics at the requested scopes for that node.
	LoadNodeData(cluster string, metrics, nodes []string, scopes []schema.MetricScope, from, to time.Time, ctx context.Context) (map[string]map[string][]*schema.JobMetric, error)
}

var metricDataRepos map[string]MetricDataRepository = map[string]MetricDataRepository{}

var JobArchivePath string

var useArchive bool

func Init(jobArchivePath string, disableArchive bool) error {
	useArchive = !disableArchive
	JobArchivePath = jobArchivePath
	for _, cluster := range config.Clusters {
		if cluster.MetricDataRepository != nil {
			var kind struct {
				Kind string `json:"kind"`
			}
			if err := json.Unmarshal(cluster.MetricDataRepository, &kind); err != nil {
				return err
			}

			var mdr MetricDataRepository
			switch kind.Kind {
			case "cc-metric-store":
				mdr = &CCMetricStore{}
			case "influxdb":
				mdr = &InfluxDBv2DataRepository{}
			case "test":
				mdr = &TestMetricDataRepository{}
			default:
				return fmt.Errorf("unkown metric data repository '%s' for cluster '%s'", kind.Kind, cluster.Name)
			}

			if err := mdr.Init(cluster.MetricDataRepository); err != nil {
				return err
			}
			metricDataRepos[cluster.Name] = mdr
		}
	}
	return nil
}

var cache *lrucache.Cache = lrucache.New(128 * 1024 * 1024)

// Fetches the metric data for a job.
func LoadData(job *schema.Job, metrics []string, scopes []schema.MetricScope, ctx context.Context) (schema.JobData, error) {
	data := cache.Get(cacheKey(job, metrics, scopes), func() (_ interface{}, ttl time.Duration, size int) {
		var jd schema.JobData
		var err error
		if job.State == schema.JobStateRunning ||
			job.MonitoringStatus == schema.MonitoringStatusRunningOrArchiving ||
			!useArchive {
			repo, ok := metricDataRepos[job.Cluster]
			if !ok {
				return fmt.Errorf("no metric data repository configured for '%s'", job.Cluster), 0, 0
			}

			if scopes == nil {
				scopes = append(scopes, schema.MetricScopeNode)
			}

			if metrics == nil {
				cluster := config.GetCluster(job.Cluster)
				for _, mc := range cluster.MetricConfig {
					metrics = append(metrics, mc.Name)
				}
			}

			jd, err = repo.LoadData(job, metrics, scopes, ctx)
			if err != nil {
				if len(jd) != 0 {
					log.Errorf("partial error: %s", err.Error())
				} else {
					return err, 0, 0
				}
			}
			size = jd.Size()
		} else {
			jd, err = loadFromArchive(job)
			if err != nil {
				return err, 0, 0
			}

			// Avoid sending unrequested data to the client:
			if metrics != nil || scopes != nil {
				if metrics == nil {
					metrics = make([]string, 0, len(jd))
					for k := range jd {
						metrics = append(metrics, k)
					}
				}

				res := schema.JobData{}
				for _, metric := range metrics {
					if perscope, ok := jd[metric]; ok {
						if len(perscope) > 1 {
							subset := make(map[schema.MetricScope]*schema.JobMetric)
							for _, scope := range scopes {
								if jm, ok := perscope[scope]; ok {
									subset[scope] = jm
								}
							}

							if len(subset) > 0 {
								perscope = subset
							}
						}

						res[metric] = perscope
					}
				}
				jd = res
			}
			size = 1 // loadFromArchive() caches in the same cache.
		}

		ttl = 5 * time.Hour
		if job.State == schema.JobStateRunning {
			ttl = 2 * time.Minute
		}

		prepareJobData(job, jd, scopes)
		return jd, ttl, size
	})

	if err, ok := data.(error); ok {
		return nil, err
	}

	return data.(schema.JobData), nil
}

// Used for the jobsFootprint GraphQL-Query. TODO: Rename/Generalize.
func LoadAverages(job *schema.Job, metrics []string, data [][]schema.Float, ctx context.Context) error {
	if job.State != schema.JobStateRunning && useArchive {
		return loadAveragesFromArchive(job, metrics, data)
	}

	repo, ok := metricDataRepos[job.Cluster]
	if !ok {
		return fmt.Errorf("no metric data repository configured for '%s'", job.Cluster)
	}

	stats, err := repo.LoadStats(job, metrics, ctx)
	if err != nil {
		return err
	}

	for i, m := range metrics {
		nodes, ok := stats[m]
		if !ok {
			data[i] = append(data[i], schema.NaN)
			continue
		}

		sum := 0.0
		for _, node := range nodes {
			sum += node.Avg
		}
		data[i] = append(data[i], schema.Float(sum))
	}

	return nil
}

// Used for the node/system view. Returns a map of nodes to a map of metrics.
func LoadNodeData(cluster string, metrics, nodes []string, scopes []schema.MetricScope, from, to time.Time, ctx context.Context) (map[string]map[string][]*schema.JobMetric, error) {
	repo, ok := metricDataRepos[cluster]
	if !ok {
		return nil, fmt.Errorf("no metric data repository configured for '%s'", cluster)
	}

	if metrics == nil {
		for _, m := range config.GetCluster(cluster).MetricConfig {
			metrics = append(metrics, m.Name)
		}
	}

	data, err := repo.LoadNodeData(cluster, metrics, nodes, scopes, from, to, ctx)
	if err != nil {
		if len(data) != 0 {
			log.Errorf("partial error: %s", err.Error())
		} else {
			return nil, err
		}
	}

	if data == nil {
		return nil, fmt.Errorf("the metric data repository for '%s' does not support this query", cluster)
	}

	return data, nil
}

func cacheKey(job *schema.Job, metrics []string, scopes []schema.MetricScope) string {
	// Duration and StartTime do not need to be in the cache key as StartTime is less unique than
	// job.ID and the TTL of the cache entry makes sure it does not stay there forever.
	return fmt.Sprintf("%d(%s):[%v],[%v]",
		job.ID, job.State, metrics, scopes)
}

// For /monitoring/job/<job> and some other places, flops_any and mem_bw need to be available at the scope 'node'.
// If a job has a lot of nodes, statisticsSeries should be available so that a min/mean/max Graph can be used instead of
// a lot of single lines.
func prepareJobData(job *schema.Job, jobData schema.JobData, scopes []schema.MetricScope) {
	const maxSeriesSize int = 15
	for _, scopes := range jobData {
		for _, jm := range scopes {
			if jm.StatisticsSeries != nil || len(jm.Series) <= maxSeriesSize {
				continue
			}

			jm.AddStatisticsSeries()
		}
	}

	nodeScopeRequested := false
	for _, scope := range scopes {
		if scope == schema.MetricScopeNode {
			nodeScopeRequested = true
		}
	}

	if nodeScopeRequested {
		jobData.AddNodeScope("flops_any")
		jobData.AddNodeScope("mem_bw")
	}
}