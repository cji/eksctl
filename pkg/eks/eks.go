package eks

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/kris-nova/logger"
	"github.com/pkg/errors"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/request"
	awseks "github.com/aws/aws-sdk-go/service/eks"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"

	api "github.com/weaveworks/eksctl/pkg/apis/eksctl.io/v1alpha5"
	"github.com/weaveworks/eksctl/pkg/cfn/manager"
	"github.com/weaveworks/eksctl/pkg/printers"
	"github.com/weaveworks/eksctl/pkg/utils/waiters"
	"github.com/weaveworks/eksctl/pkg/vpc"
)

// DescribeControlPlane describes the cluster control plane
func (c *ClusterProvider) DescribeControlPlane(cl *api.ClusterMeta) (*awseks.Cluster, error) {
	input := &awseks.DescribeClusterInput{
		Name: &cl.Name,
	}
	output, err := c.Provider.EKS().DescribeCluster(input)
	if err != nil {
		return nil, errors.Wrap(err, "unable to describe cluster control plane")
	}
	return output.Cluster, nil
}

// DescribeControlPlaneMustBeActive describes the cluster control plane and checks if status is active
func (c *ClusterProvider) DescribeControlPlaneMustBeActive(cl *api.ClusterMeta) (*awseks.Cluster, error) {
	cluster, err := c.DescribeControlPlane(cl)
	if err != nil {
		return nil, errors.Wrap(err, "unable to describe cluster control plane")
	}
	if *cluster.Status != awseks.ClusterStatusActive {
		return nil, fmt.Errorf("status of cluster %q is %q, has to be %q", *cluster.Name, *cluster.Status, awseks.ClusterStatusActive)
	}

	return cluster, nil
}

// GetCredentials retrieves cluster endpoint and the certificate authority data
func (c *ClusterProvider) GetCredentials(spec *api.ClusterConfig) error {
	// Check the cluster exists and is active
	cluster, err := c.DescribeControlPlaneMustBeActive(spec.Metadata)
	if err != nil {
		return err
	}
	logger.Debug("cluster = %#v", cluster)

	data, err := base64.StdEncoding.DecodeString(*cluster.CertificateAuthority.Data)
	if err != nil {
		return errors.Wrap(err, "decoding certificate authority data")
	}

	if spec.Status == nil {
		spec.Status = &api.ClusterStatus{}
	}

	spec.Status.Endpoint = *cluster.Endpoint
	spec.Status.CertificateAuthorityData = data
	spec.Status.ARN = *cluster.Arn

	c.Status.cachedClusterInfo = cluster

	return nil
}

// ControlPlaneVersion returns cached version (EKS API)
func (c *ClusterProvider) ControlPlaneVersion() string {
	if c.Status.cachedClusterInfo == nil || c.Status.cachedClusterInfo.Version == nil {
		return ""
	}
	return *c.Status.cachedClusterInfo.Version
}

// GetClusterVPC retrieves the VPC configuration
func (c *ClusterProvider) GetClusterVPC(spec *api.ClusterConfig) error {
	stack, err := c.NewStackManager(spec).DescribeClusterStack()
	if err != nil {
		return err
	}

	return vpc.UseFromCluster(c.Provider, stack, spec)
}

// ListClusters display details of all the EKS cluster in your account
func (c *ClusterProvider) ListClusters(clusterName string, chunkSize int, output string, eachRegion bool) error {
	// NOTE: this needs to be reworked in the future so that the functionality
	// is combined. This require the ability to return details of all clusters
	// in a single call.
	printer, err := printers.NewPrinter(output)
	if err != nil {
		return err
	}

	if clusterName != "" {
		if output == "table" {
			addSummaryTableColumns(printer.(*printers.TablePrinter))
		}
		return c.doGetCluster(clusterName, printer)
	}

	if output == "table" {
		addListTableColumns(printer.(*printers.TablePrinter))
	}
	allClusters := []*api.ClusterMeta{}
	if err := c.doListClusters(int64(chunkSize), printer, &allClusters, eachRegion); err != nil {
		return err
	}
	return printer.PrintObjWithKind("clusters", allClusters, os.Stdout)
}

func (c *ClusterProvider) getClustersRequest(chunkSize int64, nextToken string) ([]*string, *string, error) {
	input := &awseks.ListClustersInput{MaxResults: &chunkSize}
	if nextToken != "" {
		input = input.SetNextToken(nextToken)
	}
	output, err := c.Provider.EKS().ListClusters(input)
	if err != nil {
		return nil, nil, errors.Wrap(err, "listing control planes")
	}
	return output.Clusters, output.NextToken, nil
}

func (c *ClusterProvider) doListClusters(chunkSize int64, printer printers.OutputPrinter, allClusters *[]*api.ClusterMeta, eachRegion bool) error {
	if eachRegion {
		// reset region and re-create the client, then make a recursive call
		for _, region := range api.SupportedRegions() {
			spec := &api.ProviderConfig{
				Region:      region,
				Profile:     c.Provider.Profile(),
				WaitTimeout: c.Provider.WaitTimeout(),
			}
			if err := New(spec, nil).doListClusters(chunkSize, printer, allClusters, false); err != nil {
				logger.Critical("error listing clusters in %q region: %s", region, err.Error())
			}
		}
		return nil
	}

	token := ""
	for {
		clusters, nextToken, err := c.getClustersRequest(chunkSize, token)
		if err != nil {
			return err
		}

		for _, clusterName := range clusters {
			*allClusters = append(*allClusters, &api.ClusterMeta{
				Name:   *clusterName,
				Region: c.Provider.Region(),
			})
		}

		if api.IsSetAndNonEmptyString(nextToken) {
			token = *nextToken
		} else {
			break
		}
	}

	return nil
}

func (c *ClusterProvider) doGetCluster(clusterName string, printer printers.OutputPrinter) error {
	input := &awseks.DescribeClusterInput{
		Name: &clusterName,
	}
	output, err := c.Provider.EKS().DescribeCluster(input)
	if err != nil {
		return errors.Wrapf(err, "unable to describe control plane %q", clusterName)
	}
	logger.Debug("cluster = %#v", output)

	clusters := []*awseks.Cluster{output.Cluster} // TODO: in the future this will have multiple clusters
	if err := printer.PrintObjWithKind("clusters", clusters, os.Stdout); err != nil {
		return err
	}

	if *output.Cluster.Status == awseks.ClusterStatusActive {

		if logger.Level >= 4 {
			spec := &api.ClusterConfig{Metadata: &api.ClusterMeta{Name: clusterName}}
			stacks, err := c.NewStackManager(spec).ListStacks(fmt.Sprintf("^(eksclt|EKS)-%s-.*$", clusterName))
			if err != nil {
				return errors.Wrapf(err, "listing CloudFormation stack for %q", clusterName)
			}
			for _, s := range stacks {
				logger.Debug("stack = %#v", *s)
			}
		}
	}
	return nil
}

// WaitForControlPlane waits till the control plane is ready
func (c *ClusterProvider) WaitForControlPlane(id *api.ClusterMeta, clientSet *kubernetes.Clientset) error {
	if _, err := clientSet.ServerVersion(); err == nil {
		return nil
	}

	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	timer := time.NewTimer(c.Provider.WaitTimeout())
	defer timer.Stop()

	for {
		select {
		case <-ticker.C:
			_, err := clientSet.ServerVersion()
			if err == nil {
				return nil
			}
			logger.Debug("control plane not ready yet – %s", err.Error())
		case <-timer.C:
			return fmt.Errorf("timed out waiting for control plane %q after %s", id.Name, c.Provider.WaitTimeout())
		}
	}
}

type updateClusterConfigTask struct {
	info string
	spec *api.ClusterConfig
	call func(*api.ClusterConfig) error
}

func (t *updateClusterConfigTask) Describe() string { return t.info }
func (t *updateClusterConfigTask) Do(errs chan error) error {
	err := t.call(t.spec)
	close(errs)
	return err
}

// GetCurrentClusterConfigForLogging fetches current cluster logging configuration as two sets - dsiable and enanaled types
func (c *ClusterProvider) GetCurrentClusterConfigForLogging(cl *api.ClusterMeta) (sets.String, sets.String, error) {
	enabled := sets.NewString()
	disabled := sets.NewString()

	cluster, err := c.DescribeControlPlaneMustBeActive(cl)
	if err != nil {
		return nil, nil, errors.Wrap(err, "unable to retrieve current cluster logging configuration")
	}

	for _, i := range cluster.Logging.ClusterLogging {
		for _, t := range i.Types {
			if t == nil {
				return nil, nil, fmt.Errorf("unexpected response from EKS API - nil string")
			}
			if api.IsEnabled(i.Enabled) {
				enabled.Insert(*t)
			}
			if api.IsDisabled(i.Enabled) {
				disabled.Insert(*t)
			}
		}
	}
	return enabled, disabled, nil
}

// UpdateClusterConfigForLogging calls UpdateClusterConfig to enable logging
func (c *ClusterProvider) UpdateClusterConfigForLogging(cfg *api.ClusterConfig) error {
	enabled := sets.NewString()
	disabled := sets.NewString()
	if cfg.CloudWatch != nil && cfg.CloudWatch.ClusterLogging != nil {
		enabled.Insert(cfg.CloudWatch.ClusterLogging.EnableTypes...)
	}

	for _, logType := range api.SupportedCloudWatchClusterLoggingTypes() {
		if !enabled.Has(logType) {
			disabled.Insert((logType))
		}
	}

	input := &awseks.UpdateClusterConfigInput{
		Name: &cfg.Metadata.Name,
		Logging: &awseks.Logging{
			ClusterLogging: []*awseks.LogSetup{
				{
					Enabled: api.Enabled(),
					Types:   aws.StringSlice(enabled.List()),
				},
				{
					Enabled: api.Disabled(),
					Types:   aws.StringSlice(disabled.List()),
				},
			},
		},
	}

	output, err := c.Provider.EKS().UpdateClusterConfig(input)
	if err != nil {
		return err
	}
	if err := c.waitForUpdateToSucceed(cfg.Metadata.Name, output.Update); err != nil {
		return err
	}

	describeEnabledTypes := "no types enabled"
	if len(enabled.List()) > 0 {
		describeEnabledTypes = fmt.Sprintf("enabled types: %s", strings.Join(enabled.List(), ", "))
	}

	describeDisabledTypes := "no types disabled"
	if len(disabled.List()) > 0 {
		describeDisabledTypes = fmt.Sprintf("disabled types: %s", strings.Join(disabled.List(), ", "))
	}

	logger.Success("configured CloudWatch logging for cluster %q in %q (%s & %s)",
		cfg.Metadata.Name, cfg.Metadata.Region, describeEnabledTypes, describeDisabledTypes,
	)
	return nil
}

// UpdateClusterConfigTasks returns all tasks for updating cluster configuration
func (c *ClusterProvider) UpdateClusterConfigTasks(cfg *api.ClusterConfig) *manager.TaskTree {
	tasks := &manager.TaskTree{Parallel: false}

	tasks.Append(&updateClusterConfigTask{
		info: "update CloudWatch logging configurtaion",
		spec: cfg,
		call: c.UpdateClusterConfigForLogging,
	})

	return tasks
}

// UpdateClusterVersion calls eks.UpdateClusterVersion and updates to cfg.Metadata.Version,
// it will return update ID along with an error (if it occurrs)
func (c *ClusterProvider) UpdateClusterVersion(cfg *api.ClusterConfig) (*awseks.Update, error) {
	input := &awseks.UpdateClusterVersionInput{
		Name:    &cfg.Metadata.Name,
		Version: &cfg.Metadata.Version,
	}
	output, err := c.Provider.EKS().UpdateClusterVersion(input)
	if err != nil {
		return nil, err
	}
	return output.Update, nil
}

// UpdateClusterVersionBlocking calls UpdateClusterVersion and blocks until update
// operation is successful
func (c *ClusterProvider) UpdateClusterVersionBlocking(cfg *api.ClusterConfig) error {
	id, err := c.UpdateClusterVersion(cfg)
	if err != nil {
		return err
	}

	return c.waitForUpdateToSucceed(cfg.Metadata.Name, id)
}

func (c *ClusterProvider) waitForUpdateToSucceed(clusterName string, update *awseks.Update) error {
	newRequest := func() *request.Request {
		input := &awseks.DescribeUpdateInput{
			Name:     &clusterName,
			UpdateId: update.Id,
		}
		req, _ := c.Provider.EKS().DescribeUpdateRequest(input)
		return req
	}

	acceptors := waiters.MakeAcceptors(
		"Update.Status",
		awseks.UpdateStatusSuccessful,
		[]string{
			awseks.UpdateStatusCancelled,
			awseks.UpdateStatusFailed,
		},
	)

	msg := fmt.Sprintf("waiting for requested %q in cluster %q to succeed", *update.Type, clusterName)

	return waiters.Wait(clusterName, msg, acceptors, newRequest, c.Provider.WaitTimeout(), nil)
}

func addSummaryTableColumns(printer *printers.TablePrinter) {
	printer.AddColumn("NAME", func(c *awseks.Cluster) string {
		return *c.Name
	})
	printer.AddColumn("VERSION", func(c *awseks.Cluster) string {
		return *c.Version
	})
	printer.AddColumn("STATUS", func(c *awseks.Cluster) string {
		return *c.Status
	})
	printer.AddColumn("CREATED", func(c *awseks.Cluster) string {
		return c.CreatedAt.Format(time.RFC3339)
	})
	printer.AddColumn("VPC", func(c *awseks.Cluster) string {
		return *c.ResourcesVpcConfig.VpcId
	})
	printer.AddColumn("SUBNETS", func(c *awseks.Cluster) string {
		subnets := sets.NewString()
		for _, subnetid := range c.ResourcesVpcConfig.SubnetIds {
			if api.IsSetAndNonEmptyString(subnetid) {
				subnets.Insert(*subnetid)
			}
		}
		return strings.Join(subnets.List(), ",")
	})
	printer.AddColumn("SECURITYGROUPS", func(c *awseks.Cluster) string {
		groups := sets.NewString()
		for _, sg := range c.ResourcesVpcConfig.SecurityGroupIds {
			if api.IsSetAndNonEmptyString(sg) {
				groups.Insert(*sg)
			}
		}
		return strings.Join(groups.List(), ",")
	})
}

func addListTableColumns(printer *printers.TablePrinter) {
	printer.AddColumn("NAME", func(c *api.ClusterMeta) string {
		return c.Name
	})
	printer.AddColumn("REGION", func(c *api.ClusterMeta) string {
		return c.Region
	})
}
