// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package host // import "github.com/open-telemetry/opentelemetry-collector-contrib/receiver/awscontainerinsightreceiver/internal/host"

import (
	"context"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"go.uber.org/zap"

	ci "github.com/open-telemetry/opentelemetry-collector-contrib/internal/aws/containerinsight"
)

const (
	clusterNameKey          = "container-insight-eks-cluster-name"
	clusterNameTagKeyPrefix = "kubernetes.io/cluster/"
	autoScalingGroupNameTag = "aws:autoscaling:groupName"
)

type ec2TagsClient interface {
	DescribeTags(ctx context.Context, input *ec2.DescribeTagsInput, optFns ...func(options *ec2.Options)) (*ec2.DescribeTagsOutput, error)
}

type ec2TagsProvider interface {
	getClusterName() string
	getAutoScalingGroupName() string
}

type ec2Tags struct {
	refreshInterval       time.Duration
	maxJitterTime         time.Duration
	containerOrchestrator string
	instanceID            string
	client                ec2TagsClient
	clusterName           string
	autoScalingGroupName  string
	isSuccess             chan bool // only used in testing
	logger                *zap.Logger
}

type ec2TagsOption func(*ec2Tags)

func newEC2Tags(ctx context.Context, cfg aws.Config, instanceID, region, containerOrchestrator string,
	refreshInterval time.Duration, logger *zap.Logger, options ...ec2TagsOption,
) ec2TagsProvider {
	cfg.Region = region

	et := &ec2Tags{
		instanceID:            instanceID,
		client:                ec2.NewFromConfig(cfg),
		refreshInterval:       refreshInterval,
		maxJitterTime:         3 * time.Second,
		logger:                logger,
		containerOrchestrator: containerOrchestrator,
	}

	for _, opt := range options {
		opt(et)
	}

	shouldRefresh := func() bool {
		if containerOrchestrator == ci.EKS {
			// stop once we get the cluster name
			return et.clusterName == ""
		}
		return et.autoScalingGroupName == ""
	}

	go RefreshUntil(ctx, et.refresh, et.refreshInterval, shouldRefresh, et.maxJitterTime)

	return et
}

func (et *ec2Tags) fetchEC2Tags(ctx context.Context) map[string]string {
	et.logger.Info("Fetch ec2 tags to detect cluster name and auto scaling group name", zap.String("instanceId", et.instanceID))
	tags := make(map[string]string)

	tagFilters := []ec2types.Filter{
		{
			Name:   aws.String("resource-type"),
			Values: []string{"instance"},
		},
		{
			Name:   aws.String("resource-id"),
			Values: []string{et.instanceID},
		},
	}

	input := &ec2.DescribeTagsInput{
		Filters: tagFilters,
	}

	for {
		result, err := et.client.DescribeTags(ctx, input)
		if err != nil {
			et.logger.Warn("Fail to call ec2 DescribeTags", zap.Error(err), zap.String("instanceId", et.instanceID))
			break
		}

		for _, tag := range result.Tags {
			key := *tag.Key
			tags[key] = *tag.Value
			if strings.HasPrefix(key, clusterNameTagKeyPrefix) && *tag.Value == "owned" {
				tags[clusterNameKey] = key[len(clusterNameTagKeyPrefix):]
			}
		}

		if result.NextToken == nil {
			break
		}
		input.NextToken = result.NextToken
	}

	return tags
}

func (et *ec2Tags) getClusterName() string {
	return et.clusterName
}

func (et *ec2Tags) getAutoScalingGroupName() string {
	return et.autoScalingGroupName
}

func (et *ec2Tags) refresh(ctx context.Context) {
	tags := et.fetchEC2Tags(ctx)
	et.logger.Info("Fetch ec2 tags successfully")
	et.clusterName = tags[clusterNameKey]
	et.autoScalingGroupName = tags[autoScalingGroupNameTag]
	et.logger.Info("Fetch ec2 tags to detect cluster name and auto scaling group name", zap.String("instanceId", et.autoScalingGroupName))
	et.logger.Info("Fetch ec2 tags to detect cluster name and auto scaling group name", zap.String("instanceId", et.clusterName))
	if et.containerOrchestrator == ci.ECS {
		if et.isSuccess != nil && et.autoScalingGroupName != "" {
			close(et.isSuccess)
		}
	} else {
		if et.isSuccess != nil && et.autoScalingGroupName != "" && et.clusterName != "" {
			// this will be executed only in testing
			close(et.isSuccess)
		}
	}
}
