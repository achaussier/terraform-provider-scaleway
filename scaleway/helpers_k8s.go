package scaleway

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/scaleway/scaleway-sdk-go/api/k8s/v1"
	"github.com/scaleway/scaleway-sdk-go/scw"
)

type KubeconfigStruct struct {
	APIVersion string `yaml:"apiVersion"`
	Clusters   []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
			Server                   string `yaml:"server"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster string `yaml:"cluster"`
			User    string `yaml:"user"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	Kind  string `yaml:"kind"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Token string `yaml:"token"`
		} `yaml:"user"`
	} `yaml:"users"`
}

const (
	defaultK8SClusterTimeout             = 10 * time.Minute
	defaultK8SPoolTimeout                = 10 * time.Minute
	K8SClusterWaitForPoolRequiredTimeout = 10 * time.Minute
	K8SClusterWaitForDeletedTimeout      = 10 * time.Minute
	K8SPoolWaitForReadyTimeout           = 15 * time.Minute
)

func k8sAPIWithRegion(d *schema.ResourceData, m interface{}) (*k8s.API, scw.Region, error) {
	meta := m.(*Meta)
	k8sAPI := k8s.NewAPI(meta.scwClient)

	region, err := extractRegion(d, meta)
	if err != nil {
		return nil, "", err
	}
	return k8sAPI, region, nil
}

func k8sAPIWithRegionAndID(m interface{}, id string) (*k8s.API, scw.Region, string, error) {
	meta := m.(*Meta)
	k8sAPI := k8s.NewAPI(meta.scwClient)

	region, ID, err := parseRegionalID(id)
	if err != nil {
		return nil, "", "", err
	}
	return k8sAPI, region, ID, nil
}

func k8sGetMinorVersionFromFull(version string) (string, error) {
	versionSplit := strings.Split(version, ".")
	if len(versionSplit) != 3 {
		return "", fmt.Errorf("version is not a full x.y.z version") // shoud never happen
	}

	return versionSplit[0] + "." + versionSplit[1], nil
}

// k8sGetLatestVersionFromMinor returns the latest full version (x.y.z) for a given minor version (x.y)
func k8sGetLatestVersionFromMinor(ctx context.Context, k8sAPI *k8s.API, region scw.Region, version string) (string, error) {
	versionSplit := strings.Split(version, ".")
	if len(versionSplit) != 2 {
		return "", fmt.Errorf("minor version should be like x.y not %s", version)
	}

	versionsResp, err := k8sAPI.ListVersions(&k8s.ListVersionsRequest{
		Region: region,
	}, scw.WithContext(ctx))
	if err != nil {
		return "", err
	}

	for _, v := range versionsResp.Versions {
		vSplit := strings.Split(v.Name, ".")
		if len(vSplit) != 3 {
			return "", fmt.Errorf("upstream version %s is not correctly formatted", v.Name) // should never happen
		}
		if versionSplit[0] == vSplit[0] && versionSplit[1] == vSplit[1] {
			return v.Name, nil
		}
	}
	return "", fmt.Errorf("no available upstream version found for %s", version)
}

func waitK8SCluster(ctx context.Context, k8sAPI *k8s.API, region scw.Region, clusterID string) (*k8s.Cluster, error) {
	return k8sAPI.WaitForCluster(&k8s.WaitForClusterRequest{
		ClusterID:     clusterID,
		Region:        region,
		Timeout:       scw.TimeDurationPtr(K8SClusterWaitForPoolRequiredTimeout),
		RetryInterval: DefaultWaitRetryInterval,
	}, scw.WithContext(ctx))
}

func waitK8SClusterPool(ctx context.Context, k8sAPI *k8s.API, region scw.Region, clusterID string) (*k8s.Cluster, error) {
	return k8sAPI.WaitForClusterPool(&k8s.WaitForClusterRequest{
		ClusterID:     clusterID,
		Region:        region,
		Timeout:       scw.TimeDurationPtr(K8SClusterWaitForPoolRequiredTimeout),
		RetryInterval: DefaultWaitRetryInterval,
	}, scw.WithContext(ctx))
}

func waitK8SClusterDeleted(ctx context.Context, k8sAPI *k8s.API, region scw.Region, clusterID string) error {
	cluster, err := k8sAPI.WaitForCluster(&k8s.WaitForClusterRequest{
		ClusterID:     clusterID,
		Region:        region,
		Timeout:       scw.TimeDurationPtr(K8SClusterWaitForDeletedTimeout),
		RetryInterval: DefaultWaitRetryInterval,
	}, scw.WithContext(ctx))
	if err != nil {
		if is404Error(err) {
			return nil
		}
		return err
	}

	return fmt.Errorf("cluster %s has state %s, wants %s", clusterID, cluster.Status, k8s.ClusterStatusDeleted)
}

func waitK8SPoolReady(ctx context.Context, k8sAPI *k8s.API, region scw.Region, poolID string) error {
	pool, err := k8sAPI.WaitForPool(&k8s.WaitForPoolRequest{
		PoolID:        poolID,
		Region:        region,
		Timeout:       scw.TimeDurationPtr(K8SPoolWaitForReadyTimeout),
		RetryInterval: DefaultWaitRetryInterval,
	}, scw.WithContext(ctx))

	if err != nil {
		return err
	}

	if pool.Status != k8s.PoolStatusReady {
		return fmt.Errorf("pool %s has state %s, wants %s", poolID, pool.Status, k8s.PoolStatusReady)
	}
	return nil
}

// convert a list of nodes to a list of map
func convertNodes(res *k8s.ListNodesResponse) []map[string]interface{} {
	var result []map[string]interface{}
	for _, node := range res.Nodes {
		n := make(map[string]interface{})
		n["name"] = node.Name
		n["status"] = node.Status.String()
		if node.PublicIPV4 != nil && node.PublicIPV4.String() != "<nil>" {
			n["public_ip"] = node.PublicIPV4.String()
		}
		if node.PublicIPV6 != nil && node.PublicIPV6.String() != "<nil>" {
			n["public_ip_v6"] = node.PublicIPV6.String()
		}
		result = append(result, n)
	}
	return result
}

func getNodes(ctx context.Context, k8sAPI *k8s.API, pool *k8s.Pool) ([]map[string]interface{}, error) {
	req := &k8s.ListNodesRequest{
		Region:    pool.Region,
		ClusterID: pool.ClusterID,
		PoolID:    &pool.ID,
	}

	nodes, err := k8sAPI.ListNodes(req, scw.WithAllPages(), scw.WithContext(ctx))

	if err != nil {
		return nil, err
	}

	return convertNodes(nodes), nil
}

func clusterAutoscalerConfigFlatten(cluster *k8s.Cluster) []map[string]interface{} {
	autoscalerConfig := map[string]interface{}{}
	autoscalerConfig["disable_scale_down"] = cluster.AutoscalerConfig.ScaleDownDisabled
	autoscalerConfig["scale_down_delay_after_add"] = cluster.AutoscalerConfig.ScaleDownDelayAfterAdd
	autoscalerConfig["scale_down_unneeded_time"] = cluster.AutoscalerConfig.ScaleDownUnneededTime
	autoscalerConfig["estimator"] = cluster.AutoscalerConfig.Estimator
	autoscalerConfig["expander"] = cluster.AutoscalerConfig.Expander
	autoscalerConfig["ignore_daemonsets_utilization"] = cluster.AutoscalerConfig.IgnoreDaemonsetsUtilization
	autoscalerConfig["balance_similar_node_groups"] = cluster.AutoscalerConfig.BalanceSimilarNodeGroups
	autoscalerConfig["expendable_pods_priority_cutoff"] = cluster.AutoscalerConfig.ExpendablePodsPriorityCutoff

	// need to convert a f32 to f64 without precision loss
	thresholdF64, err := strconv.ParseFloat(fmt.Sprintf("%f", cluster.AutoscalerConfig.ScaleDownUtilizationThreshold), 64)
	if err != nil {
		// should never happen
		return nil
	}
	autoscalerConfig["scale_down_utilization_threshold"] = thresholdF64
	autoscalerConfig["max_graceful_termination_sec"] = cluster.AutoscalerConfig.MaxGracefulTerminationSec

	return []map[string]interface{}{autoscalerConfig}
}

func clusterOpenIDConnectConfigFlatten(cluster *k8s.Cluster) []map[string]interface{} {
	openIDConnectConfig := map[string]interface{}{}
	openIDConnectConfig["issuer_url"] = cluster.OpenIDConnectConfig.IssuerURL
	openIDConnectConfig["client_id"] = cluster.OpenIDConnectConfig.ClientID
	openIDConnectConfig["username_claim"] = cluster.OpenIDConnectConfig.UsernameClaim
	openIDConnectConfig["username_prefix"] = cluster.OpenIDConnectConfig.UsernamePrefix
	openIDConnectConfig["groups_claim"] = cluster.OpenIDConnectConfig.GroupsClaim
	openIDConnectConfig["groups_prefix"] = cluster.OpenIDConnectConfig.GroupsPrefix
	openIDConnectConfig["required_claim"] = cluster.OpenIDConnectConfig.RequiredClaim

	return []map[string]interface{}{openIDConnectConfig}
}

func clusterAutoUpgradeFlatten(cluster *k8s.Cluster) []map[string]interface{} {
	autoUpgrade := map[string]interface{}{}
	autoUpgrade["enable"] = cluster.AutoUpgrade.Enabled
	autoUpgrade["maintenance_window_start_hour"] = cluster.AutoUpgrade.MaintenanceWindow.StartHour
	autoUpgrade["maintenance_window_day"] = cluster.AutoUpgrade.MaintenanceWindow.Day

	return []map[string]interface{}{autoUpgrade}
}

func poolUpgradePolicyFlatten(pool *k8s.Pool) []map[string]interface{} {
	upgradePolicy := map[string]interface{}{}
	if pool.UpgradePolicy != nil {
		upgradePolicy["max_surge"] = pool.UpgradePolicy.MaxSurge
		upgradePolicy["max_unavailable"] = pool.UpgradePolicy.MaxUnavailable
	}

	return []map[string]interface{}{upgradePolicy}
}

func expandKubeletArgs(args interface{}) map[string]string {
	kubeletArgs := map[string]string{}

	for key, value := range args.(map[string]interface{}) {
		kubeletArgs[key] = value.(string)
	}

	return kubeletArgs
}

func flattenKubeletArgs(args map[string]string) map[string]interface{} {
	kubeletArgs := map[string]interface{}{}

	for key, value := range args {
		kubeletArgs[key] = value
	}

	return kubeletArgs
}
