package sdk

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/cloudformation/cloudformationiface"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go/service/cloudwatchlogs/cloudwatchlogsiface"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/ecs/ecsiface"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/elbv2/elbv2iface"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/iam/iamiface"
	"github.com/aws/aws-sdk-go/service/secretsmanager"
	"github.com/aws/aws-sdk-go/service/secretsmanager/secretsmanageriface"
	cf "github.com/awslabs/goformation/v4/cloudformation"
	"github.com/sirupsen/logrus"

	"github.com/docker/ecs-plugin/pkg/amazon/types"
	t "github.com/docker/ecs-plugin/pkg/amazon/types"
)

type sdk struct {
	sess *session.Session
	ECS  ecsiface.ECSAPI
	EC2  ec2iface.EC2API
	ELB  elbv2iface.ELBV2API
	CW   cloudwatchlogsiface.CloudWatchLogsAPI
	IAM  iamiface.IAMAPI
	CF   cloudformationiface.CloudFormationAPI
	SM   secretsmanageriface.SecretsManagerAPI
}

func NewAPI(sess *session.Session) API {
	return sdk{
		ECS: ecs.New(sess),
		EC2: ec2.New(sess),
		ELB: elbv2.New(sess),
		CW:  cloudwatchlogs.New(sess),
		IAM: iam.New(sess),
		CF:  cloudformation.New(sess),
		SM:  secretsmanager.New(sess),
	}
}

func (s sdk) ClusterExists(ctx context.Context, name string) (bool, error) {
	logrus.Debug("Check if cluster was already created: ", name)
	clusters, err := s.ECS.DescribeClustersWithContext(ctx, &ecs.DescribeClustersInput{
		Clusters: []*string{aws.String(name)},
	})
	if err != nil {
		return false, err
	}
	return len(clusters.Clusters) > 0, nil
}

func (s sdk) CreateCluster(ctx context.Context, name string) (string, error) {
	logrus.Debug("Create cluster ", name)
	response, err := s.ECS.CreateClusterWithContext(ctx, &ecs.CreateClusterInput{ClusterName: aws.String(name)})
	if err != nil {
		return "", err
	}
	return *response.Cluster.Status, nil
}

func (s sdk) DeleteCluster(ctx context.Context, name string) error {
	logrus.Debug("Delete cluster ", name)
	response, err := s.ECS.DeleteClusterWithContext(ctx, &ecs.DeleteClusterInput{Cluster: aws.String(name)})
	if err != nil {
		return err
	}
	if *response.Cluster.Status == "INACTIVE" {
		return nil
	}
	return fmt.Errorf("Failed to delete cluster, status: %s" + *response.Cluster.Status)
}

func (s sdk) VpcExists(ctx context.Context, vpcID string) (bool, error) {
	logrus.Debug("Check if VPC exists: ", vpcID)
	_, err := s.EC2.DescribeVpcsWithContext(ctx, &ec2.DescribeVpcsInput{VpcIds: []*string{&vpcID}})
	return err == nil, err
}

func (s sdk) GetDefaultVPC(ctx context.Context) (string, error) {
	logrus.Debug("Retrieve default VPC")
	vpcs, err := s.EC2.DescribeVpcsWithContext(ctx, &ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("isDefault"),
				Values: []*string{aws.String("true")},
			},
		},
	})
	if err != nil {
		return "", err
	}
	if len(vpcs.Vpcs) == 0 {
		return "", fmt.Errorf("account has not default VPC")
	}
	return *vpcs.Vpcs[0].VpcId, nil
}

func (s sdk) GetSubNets(ctx context.Context, vpcID string) ([]string, error) {
	logrus.Debug("Retrieve SubNets")
	subnets, err := s.EC2.DescribeSubnetsWithContext(ctx, &ec2.DescribeSubnetsInput{
		DryRun: nil,
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("vpc-id"),
				Values: []*string{aws.String(vpcID)},
			},
			{
				Name:   aws.String("default-for-az"),
				Values: []*string{aws.String("true")},
			},
		},
	})
	if err != nil {
		return nil, err
	}

	ids := []string{}
	for _, subnet := range subnets.Subnets {
		ids = append(ids, *subnet.SubnetId)
	}
	return ids, nil
}

func (s sdk) GetRoleArn(ctx context.Context, name string) (string, error) {
	role, err := s.IAM.GetRoleWithContext(ctx, &iam.GetRoleInput{
		RoleName: aws.String(name),
	})
	if err != nil {
		return "", err
	}
	return *role.Role.Arn, nil
}

func (s sdk) StackExists(ctx context.Context, name string) (bool, error) {
	stacks, err := s.CF.DescribeStacksWithContext(ctx, &cloudformation.DescribeStacksInput{
		StackName: aws.String(name),
	})
	if err != nil {
		// FIXME doesn't work as expected
		return false, nil
	}
	return len(stacks.Stacks) > 0, nil
}

func (s sdk) CreateStack(ctx context.Context, name string, template *cf.Template, parameters map[string]string) error {
	logrus.Debug("Create CloudFormation stack")
	json, err := template.JSON()
	if err != nil {
		return err
	}

	param := []*cloudformation.Parameter{}
	for name, value := range parameters {
		param = append(param, &cloudformation.Parameter{
			ParameterKey:   aws.String(name),
			ParameterValue: aws.String(value),
		})
	}

	_, err = s.CF.CreateStackWithContext(ctx, &cloudformation.CreateStackInput{
		OnFailure:        aws.String("DELETE"),
		StackName:        aws.String(name),
		TemplateBody:     aws.String(string(json)),
		Parameters:       param,
		TimeoutInMinutes: aws.Int64(10),
		Capabilities: []*string{
			aws.String(cloudformation.CapabilityCapabilityIam),
		},
	})
	return err
}

func (s sdk) CreateChangeSet(ctx context.Context, name string, template *cf.Template, parameters map[string]string) (string, error) {
	logrus.Debug("Create CloudFormation Changeset")
	json, err := template.JSON()
	if err != nil {
		return "", err
	}

	param := []*cloudformation.Parameter{}
	for name := range parameters {
		param = append(param, &cloudformation.Parameter{
			ParameterKey:     aws.String(name),
			UsePreviousValue: aws.Bool(true),
		})
	}

	update := fmt.Sprintf("Update%s", time.Now().Format("2006-01-02-15-04-05"))
	changeset, err := s.CF.CreateChangeSetWithContext(ctx, &cloudformation.CreateChangeSetInput{
		ChangeSetName: aws.String(update),
		ChangeSetType: aws.String(cloudformation.ChangeSetTypeUpdate),
		StackName:     aws.String(name),
		TemplateBody:  aws.String(string(json)),
		Parameters:    param,
		Capabilities: []*string{
			aws.String(cloudformation.CapabilityCapabilityIam),
		},
	})
	if err != nil {
		return "", err
	}

	err = s.CF.WaitUntilChangeSetCreateCompleteWithContext(ctx, &cloudformation.DescribeChangeSetInput{
		ChangeSetName: changeset.Id,
	})
	return *changeset.Id, err
}

func (s sdk) UpdateStack(ctx context.Context, changeset string) error {
	desc, err := s.CF.DescribeChangeSetWithContext(ctx, &cloudformation.DescribeChangeSetInput{
		ChangeSetName: aws.String(changeset),
	})
	if err != nil {
		return err
	}

	if desc.StatusReason != nil && strings.HasPrefix(*desc.StatusReason, "The submitted information didn't contain changes.") {
		return nil
	}

	_, err = s.CF.ExecuteChangeSet(&cloudformation.ExecuteChangeSetInput{
		ChangeSetName: aws.String(changeset),
	})
	return err
}

func (s sdk) WaitStackComplete(ctx context.Context, name string, operation int) error {
	input := &cloudformation.DescribeStacksInput{
		StackName: aws.String(name),
	}
	switch operation {
	case t.StackCreate:
		return s.CF.WaitUntilStackCreateCompleteWithContext(ctx, input)
	case t.StackUpdate:
		return s.CF.WaitUntilStackUpdateCompleteWithContext(ctx, input)
	case t.StackDelete:
		return s.CF.WaitUntilStackDeleteCompleteWithContext(ctx, input)
	default:
		return fmt.Errorf("internal error: unexpected stack operation %d", operation)
	}
}

func (s sdk) GetStackID(ctx context.Context, name string) (string, error) {
	stacks, err := s.CF.DescribeStacksWithContext(ctx, &cloudformation.DescribeStacksInput{
		StackName: aws.String(name),
	})
	if err != nil {
		return "", err
	}
	return *stacks.Stacks[0].StackId, nil
}

func (s sdk) DescribeStackEvents(ctx context.Context, stackID string) ([]*cloudformation.StackEvent, error) {
	// Fixme implement Paginator on Events and return as a chan(events)
	events := []*cloudformation.StackEvent{}
	var nextToken *string
	for {
		resp, err := s.CF.DescribeStackEventsWithContext(ctx, &cloudformation.DescribeStackEventsInput{
			StackName: aws.String(stackID),
			NextToken: nextToken,
		})
		if err != nil {
			return nil, err
		}
		events = append(events, resp.StackEvents...)
		if resp.NextToken == nil {
			return events, nil
		}
		nextToken = resp.NextToken
	}
}

func (s sdk) DeleteStack(ctx context.Context, name string) error {
	logrus.Debug("Delete CloudFormation stack")
	_, err := s.CF.DeleteStackWithContext(ctx, &cloudformation.DeleteStackInput{
		StackName: aws.String(name),
	})
	return err
}

func (s sdk) CreateSecret(ctx context.Context, secret t.Secret) (string, error) {
	logrus.Debug("Create secret " + secret.Name)
	secretStr, err := secret.GetCredString()
	if err != nil {
		return "", err
	}

	response, err := s.SM.CreateSecret(&secretsmanager.CreateSecretInput{
		Name:         &secret.Name,
		SecretString: &secretStr,
		Description:  &secret.Description,
	})
	if err != nil {
		return "", err
	}
	return *response.ARN, nil
}

func (s sdk) InspectSecret(ctx context.Context, id string) (t.Secret, error) {
	logrus.Debug("Inspect secret " + id)
	response, err := s.SM.DescribeSecret(&secretsmanager.DescribeSecretInput{SecretId: &id})
	if err != nil {
		return t.Secret{}, err
	}
	labels := map[string]string{}
	for _, tag := range response.Tags {
		labels[*tag.Key] = *tag.Value
	}
	secret := t.Secret{
		ID:     *response.ARN,
		Name:   *response.Name,
		Labels: labels,
	}
	if response.Description != nil {
		secret.Description = *response.Description
	}
	return secret, nil
}

func (s sdk) ListSecrets(ctx context.Context) ([]t.Secret, error) {

	logrus.Debug("List secrets ...")
	response, err := s.SM.ListSecrets(&secretsmanager.ListSecretsInput{})
	if err != nil {
		return []t.Secret{}, err
	}
	var secrets []t.Secret

	for _, sec := range response.SecretList {

		labels := map[string]string{}
		for _, tag := range sec.Tags {
			labels[*tag.Key] = *tag.Value
		}
		description := ""
		if sec.Description != nil {
			description = *sec.Description
		}
		secrets = append(secrets, t.Secret{
			ID:          *sec.ARN,
			Name:        *sec.Name,
			Labels:      labels,
			Description: description,
		})
	}
	return secrets, nil
}

func (s sdk) DeleteSecret(ctx context.Context, id string, recover bool) error {
	logrus.Debug("List secrets ...")
	force := !recover
	_, err := s.SM.DeleteSecret(&secretsmanager.DeleteSecretInput{SecretId: &id, ForceDeleteWithoutRecovery: &force})
	return err
}

func (s sdk) GetLogs(ctx context.Context, name string, consumer types.LogConsumer) error {
	logGroup := fmt.Sprintf("/docker-compose/%s", name)
	var startTime int64
	for {
		var hasMore = true
		var token *string
		for hasMore {
			events, err := s.CW.FilterLogEvents(&cloudwatchlogs.FilterLogEventsInput{
				LogGroupName: aws.String(logGroup),
				NextToken:    token,
				StartTime:    aws.Int64(startTime),
			})
			if err != nil {
				return err
			}
			if events.NextToken == nil {
				hasMore = false
			} else {
				token = events.NextToken
			}

			for _, event := range events.Events {
				p := strings.Split(*event.LogStreamName, "/")
				consumer.Log(p[1], p[2], *event.Message)
				startTime = *event.IngestionTime
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (s sdk) ListTasks(ctx context.Context, cluster string, service string) ([]string, error) {
	tasks, err := s.ECS.ListTasksWithContext(ctx, &ecs.ListTasksInput{
		Cluster:     aws.String(cluster),
		ServiceName: aws.String(service),
	})
	if err != nil {
		return nil, err
	}
	arns := []string{}
	for _, arn := range tasks.TaskArns {
		arns = append(arns, *arn)
	}
	return arns, nil
}

func (s sdk) DescribeTasks(ctx context.Context, cluster string, arns ...string) ([]t.TaskStatus, error) {
	tasks, err := s.ECS.DescribeTasksWithContext(ctx, &ecs.DescribeTasksInput{
		Cluster: aws.String(cluster),
		Tasks:   aws.StringSlice(arns),
	})
	if err != nil {
		return nil, err
	}
	result := []t.TaskStatus{}
	for _, task := range tasks.Tasks {
		var networkInterface string
		for _, attachement := range task.Attachments {
			if *attachement.Type == "ElasticNetworkInterface" {
				for _, pair := range attachement.Details {
					if *pair.Name == "networkInterfaceId" {
						networkInterface = *pair.Value
					}
				}
			}
		}
		result = append(result, t.TaskStatus{
			State:            *task.LastStatus,
			Service:          strings.Replace(*task.Group, "service:", "", 1),
			NetworkInterface: networkInterface,
		})
	}
	return result, nil
}

func (s sdk) GetPublicIPs(ctx context.Context, interfaces ...string) (map[string]string, error) {
	desc, err := s.EC2.DescribeNetworkInterfaces(&ec2.DescribeNetworkInterfacesInput{
		NetworkInterfaceIds: aws.StringSlice(interfaces),
	})
	if err != nil {
		return nil, err
	}
	publicIPs := map[string]string{}
	for _, interf := range desc.NetworkInterfaces {
		if interf.Association != nil {
			publicIPs[*interf.NetworkInterfaceId] = *interf.Association.PublicIp
		}
	}
	return publicIPs, nil
}

func (s sdk) LoadBalancerExists(ctx context.Context, name string) (bool, error) {
	logrus.Debug("Check if cluster was already created: ", name)
	lbs, err := s.ELB.DescribeLoadBalancersWithContext(ctx, &elbv2.DescribeLoadBalancersInput{
		Names: []*string{aws.String(name)},
	})
	if err != nil {
		return false, err
	}
	return len(lbs.LoadBalancers) > 0, nil
}

func (s sdk) GetLoadBalancerARN(ctx context.Context, name string) (string, error) {
	logrus.Debug("Check if cluster was already created: ", name)
	lbs, err := s.ELB.DescribeLoadBalancersWithContext(ctx, &elbv2.DescribeLoadBalancersInput{
		Names: []*string{aws.String(name)},
	})
	if err != nil {
		return "", err
	}
	return *lbs.LoadBalancers[0].LoadBalancerArn, nil
}
