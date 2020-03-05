package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ecs"
)

type CloudWatchEventDetail struct {
	AutoScalingGroupName string
	EC2InstanceId        string // nolint:golint,stylecheck
	LifecycleActionToken string
	LifecycleHookName    string
	LifecycleTransition  string
	Wait                 bool
}

const (
	DetailTypeTerminateLifecycle   = "EC2 Instance-terminate Lifecycle Action"
	LifecycleTransitionTerminating = "autoscaling:EC2_INSTANCE_TERMINATING"
)

var ecsClusterRegexp = regexp.MustCompile(`\bECS_CLUSTER=([-\w]+)`) // nolint:gochecknoglobals

func main() {
	lambda.Start(handler)
}

func handler(ctx context.Context, evt *events.CloudWatchEvent) (*events.CloudWatchEvent, error) {
	if err := logEvent(evt); err != nil {
		return nil, err
	}

	if evt.DetailType != DetailTypeTerminateLifecycle {
		return nil, fmt.Errorf("`detail-type` is %q, not %q", evt.DetailType, DetailTypeTerminateLifecycle)
	}

	var evtDetail *CloudWatchEventDetail
	if err := json.Unmarshal(evt.Detail, &evtDetail); err != nil {
		return nil, err
	}

	if evtDetail.LifecycleTransition != LifecycleTransitionTerminating {
		return nil, fmt.Errorf("`LifecycleTransition` is %q, not %q",
			evtDetail.LifecycleTransition, LifecycleTransitionTerminating)
	}

	sess := newSession()

	clusterName, err := getECSClusterName(ctx, sess, evtDetail.EC2InstanceId)
	if err != nil {
		return nil, err
	}

	ecsSvc := ecs.New(sess)

	containerInstance, err := getContainerInstance(ctx, ecsSvc, clusterName, evtDetail.EC2InstanceId)
	if err != nil {
		return nil, err
	}

	if *containerInstance.Status != ecs.ContainerInstanceStatusDraining {
		if err := setStateDraining(ctx, ecsSvc, clusterName, containerInstance.ContainerInstanceArn); err != nil {
			return nil, err
		}
	}

	exists, err := taskExists(ctx, ecsSvc, clusterName, containerInstance.ContainerInstanceArn)
	if err != nil {
		return nil, err
	}

	if exists {
		if err := heartbeat(ctx, sess, evtDetail); err != nil {
			return nil, err
		}
		evtDetail.Wait = true
	} else {
		if err := complete(ctx, sess, evtDetail); err != nil {
			return nil, err
		}
		evtDetail.Wait = false
	}

	if evt.Detail, err = json.Marshal(evtDetail); err != nil {
		return nil, err
	}
	return evt, nil
}

func logEvent(evt interface{}) error {
	marshaled, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	log.Println(string(marshaled))
	return nil
}

func newSession() *session.Session {
	config := aws.NewConfig()
	if os.Getenv("VERBOSE") == "true" || os.Getenv("AWS_SAM_LOCAL") == "true" {
		config.WithLogLevel(aws.LogDebugWithHTTPBody | aws.LogDebugWithRequestErrors | aws.LogDebugWithRequestRetries)
	}
	return session.Must(session.NewSession(config))
}

func getECSClusterName(ctx context.Context, sess *session.Session, instanceID string) (string, error) {
	userData, err := getUserData(ctx, sess, instanceID)
	if err != nil {
		return "", err
	}

	matches := ecsClusterRegexp.FindStringSubmatch(userData)
	if len(matches) == 0 {
		return "", errors.New("`UserData` does not have `ECS_CLUSTER=...`")
	}
	return matches[1], nil
}

func getUserData(ctx context.Context, sess *session.Session, instanceID string) (string, error) {
	output, err := ec2.New(sess).DescribeInstanceAttributeWithContext(ctx, &ec2.DescribeInstanceAttributeInput{
		InstanceId: &instanceID,
		Attribute:  aws.String(ec2.InstanceAttributeNameUserData),
	})
	if err != nil {
		return "", err
	}

	if output.UserData.Value == nil {
		return "", fmt.Errorf("instance %q does not have UserData", instanceID)
	}

	userData, err := base64.StdEncoding.DecodeString(*output.UserData.Value)
	if err != nil {
		return "", err
	}

	return string(userData), nil
}

func getContainerInstance(
	ctx context.Context, svc *ecs.ECS, clusterName string, instanceID string) (*ecs.ContainerInstance, error) {
	input := &ecs.ListContainerInstancesInput{Cluster: &clusterName}
	var arrayOfArns [][]*string
	fn := func(output *ecs.ListContainerInstancesOutput, _ bool) bool {
		if len(output.ContainerInstanceArns) > 0 {
			arrayOfArns = append(arrayOfArns, output.ContainerInstanceArns)
		}
		return true
	}
	if err := svc.ListContainerInstancesPagesWithContext(ctx, input, fn); err != nil {
		return nil, err
	}

	for _, arns := range arrayOfArns {
		output, err := svc.DescribeContainerInstancesWithContext(ctx, &ecs.DescribeContainerInstancesInput{
			Cluster:            &clusterName,
			ContainerInstances: arns,
		})
		if err != nil {
			return nil, err
		}
		for _, containerInstance := range output.ContainerInstances {
			if *containerInstance.Ec2InstanceId == instanceID {
				return containerInstance, nil
			}
		}
	}

	return nil, fmt.Errorf("%q does not have %q", clusterName, instanceID)
}

func setStateDraining(
	ctx context.Context, svc *ecs.ECS, clusterName string, containerInstanceArn *string) error {
	_, err := svc.UpdateContainerInstancesStateWithContext(ctx, &ecs.UpdateContainerInstancesStateInput{
		Cluster:            &clusterName,
		ContainerInstances: []*string{containerInstanceArn},
		Status:             aws.String(ecs.ContainerInstanceStatusDraining),
	})
	return err
}

func taskExists(ctx context.Context, svc *ecs.ECS, clusterName string, containerInstanceArn *string) (bool, error) {
	output, err := svc.ListTasksWithContext(ctx, &ecs.ListTasksInput{
		Cluster:           &clusterName,
		ContainerInstance: containerInstanceArn,
		DesiredStatus:     aws.String("RUNNING"),
	})
	if err != nil {
		return false, err
	}
	if len(output.TaskArns) > 0 {
		return true, nil
	}

	input := &ecs.ListTasksInput{
		Cluster:           &clusterName,
		ContainerInstance: containerInstanceArn,
		DesiredStatus:     aws.String("STOPPED"),
	}
	var arrayOfArns [][]*string
	fn := func(output *ecs.ListTasksOutput, _ bool) bool {
		if len(output.TaskArns) > 0 {
			arrayOfArns = append(arrayOfArns, output.TaskArns)
		}
		return true
	}
	if err := svc.ListTasksPagesWithContext(ctx, input, fn); err != nil {
		return false, err
	}

	for _, arns := range arrayOfArns {
		output, err := svc.DescribeTasksWithContext(ctx, &ecs.DescribeTasksInput{
			Cluster: &clusterName,
			Tasks:   arns,
		})
		if err != nil {
			return false, err
		}
		for _, task := range output.Tasks {
			if *task.LastStatus == "RUNNING" {
				return true, nil
			}
		}
	}

	return false, nil
}

func heartbeat(ctx context.Context, sess *session.Session, detail *CloudWatchEventDetail) error {
	svc := autoscaling.New(sess)
	_, err := svc.RecordLifecycleActionHeartbeatWithContext(ctx, &autoscaling.RecordLifecycleActionHeartbeatInput{
		AutoScalingGroupName: &detail.AutoScalingGroupName,
		LifecycleActionToken: &detail.LifecycleActionToken,
		LifecycleHookName:    &detail.LifecycleHookName,
	})
	return err
}

func complete(ctx context.Context, sess *session.Session, detail *CloudWatchEventDetail) error {
	svc := autoscaling.New(sess)
	_, err := svc.CompleteLifecycleActionWithContext(ctx, &autoscaling.CompleteLifecycleActionInput{
		AutoScalingGroupName:  &detail.AutoScalingGroupName,
		LifecycleActionResult: aws.String("CONTINUE"),
		LifecycleActionToken:  &detail.LifecycleActionToken,
		LifecycleHookName:     &detail.LifecycleHookName,
	})
	return err
}
