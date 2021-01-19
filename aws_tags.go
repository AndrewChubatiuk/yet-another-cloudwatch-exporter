package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go/service/apigateway"
	"github.com/aws/aws-sdk-go/service/apigateway/apigatewayiface"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/autoscaling/autoscalingiface"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/ec2/ec2iface"
	r "github.com/aws/aws-sdk-go/service/resourcegroupstaggingapi"
	"github.com/aws/aws-sdk-go/service/resourcegroupstaggingapi/resourcegroupstaggingapiiface"
	log "github.com/sirupsen/logrus"
)

type tagsData struct {
	ID        *string
	Matcher   *string
	Tags      []*tag
	Namespace *string
	Region    *string
}

// https://docs.aws.amazon.com/sdk-for-go/api/service/resourcegroupstaggingapi/resourcegroupstaggingapiiface/
type tagsInterface struct {
	client           resourcegroupstaggingapiiface.ResourceGroupsTaggingAPIAPI
	asgClient        autoscalingiface.AutoScalingAPI
	apiGatewayClient apigatewayiface.APIGatewayAPI
	ec2Client        ec2iface.EC2API
}

func createSession(roleArn string, config *aws.Config) *session.Session {
	sess, err := session.NewSession()
	if err != nil {
		log.Fatalf("Failed to create session due to %v", err)
	}
	if roleArn != "" {
		config.Credentials = stscreds.NewCredentials(sess, roleArn)
	}
	return sess
}

func createTagSession(region *string, roleArn string) *r.ResourceGroupsTaggingAPI {
	maxResourceGroupTaggingRetries := 5
	config := &aws.Config{Region: region, MaxRetries: &maxResourceGroupTaggingRetries}
	if *fips {
		// ToDo: Resource Groups Tagging API does not have FIPS compliant endpoints
		// https://docs.aws.amazon.com/general/latest/gr/arg.html
		// endpoint := fmt.Sprintf("https://tagging-fips.%s.amazonaws.com", *region)
		// config.Endpoint = aws.String(endpoint)
	}
	return r.New(createSession(roleArn, config), config)
}

func createASGSession(region *string, roleArn string) autoscalingiface.AutoScalingAPI {
	maxAutoScalingAPIRetries := 5
	config := &aws.Config{Region: region, MaxRetries: &maxAutoScalingAPIRetries}
	if *fips {
		// ToDo: Autoscaling does not have a FIPS endpoint
		// https://docs.aws.amazon.com/general/latest/gr/autoscaling_region.html
		// endpoint := fmt.Sprintf("https://autoscaling-plans-fips.%s.amazonaws.com", *region)
		// config.Endpoint = aws.String(endpoint)
	}
	return autoscaling.New(createSession(roleArn, config), config)
}

func createEC2Session(region *string, roleArn string) ec2iface.EC2API {
	maxEC2APIRetries := 10
	config := &aws.Config{Region: region, MaxRetries: &maxEC2APIRetries}
	if *fips {
		// https://docs.aws.amazon.com/general/latest/gr/ec2-service.html
		endpoint := fmt.Sprintf("https://ec2-fips.%s.amazonaws.com", *region)
		config.Endpoint = aws.String(endpoint)
	}
	return ec2.New(createSession(roleArn, config), config)
}

func createAPIGatewaySession(region *string, roleArn string) apigatewayiface.APIGatewayAPI {
	sess, err := session.NewSession()
	if err != nil {
		log.Fatal(err)
	}
	maxApiGatewaygAPIRetries := 5
	config := &aws.Config{Region: region, MaxRetries: &maxApiGatewaygAPIRetries}
	if roleArn != "" {
		config.Credentials = stscreds.NewCredentials(sess, roleArn)
	}
	if *fips {
		// https://docs.aws.amazon.com/general/latest/gr/apigateway.html
		endpoint := fmt.Sprintf("https://apigateway-fips.%s.amazonaws.com", *region)
		config.Endpoint = aws.String(endpoint)
	}
	return apigateway.New(sess, config)
}

func (iface tagsInterface) get(job discovery, region string) (resources []*tagsData, err error) {
	switch job.Namespace {
	case "AWS/AutoScaling":
		return iface.getTaggedAutoscalingGroups(job, region)
	case "AWS/EC2Spot":
		return iface.getTaggedEC2SpotInstances(job, region)
	}
	var inputParams r.GetResourcesInput
	nsConfig := supportedNamespaces[job.Namespace]
	resourceTypeFilters := nsConfig.Resources
	var filters []*string
	for _, filter := range resourceTypeFilters {
		filters = append(filters, aws.String(filter))
	}
	inputParams.ResourceTypeFilters = filters
	c := iface.client
	ctx := context.Background()
	pageNum := 0
	if len(nsConfig.Resources) > 0 {
		err = c.GetResourcesPagesWithContext(ctx, &inputParams, func(page *r.GetResourcesOutput, lastPage bool) bool {
			pageNum++
			resourceGroupTaggingAPICounter.Inc()
			for _, resourceTagMapping := range page.ResourceTagMappingList {
				resource := tagsData{
					ID:        resourceTagMapping.ResourceARN,
					Namespace: &job.Namespace,
					Region:    &region,
				}
				for _, t := range resourceTagMapping.Tags {
					resource.Tags = append(resource.Tags, &tag{Key: *t.Key, Value: *t.Value})
				}

				if resource.filterThroughTags(job.FilterTags) {
					resources = append(resources, &resource)
				}
			}
			return pageNum < 100
		})
	}
	if nsConfig.GlobalMetrics {
		resources = append(resources, &tagsData{
			Namespace: &job.Namespace,
			Region:    &region,
		})
	}

	switch job.Namespace {
	case "AWS/ApiGateway":
		// Get all the api gateways from aws
		apiGateways, errGet := iface.getTaggedApiGateway()
		if errGet != nil {
			log.Errorf("tagsInterface.get: apigateway: getTaggedApiGateway: %v", errGet)
			return resources, errGet
		}
		var filteredResources []*tagsData
		for _, r := range resources {
			// For each tagged resource, find the associated restApi
			// And swap out the ID with the name
			if strings.Contains(*r.ID, "/restapis") {
				restApiId := strings.Split(*r.ID, "/")[2]
				for _, apiGateway := range apiGateways.Items {
					if *apiGateway.Id == restApiId {
						r.Matcher = apiGateway.Name
					}
				}
				if r.Matcher == nil {
					log.Errorf("tagsInterface.get: apigateway: resource=%s restApiId=%s could not find gateway", *r.ID, restApiId)
					continue // exclude resource to avoid crash later
				}
				filteredResources = append(filteredResources, r)
			}
		}
		resources = filteredResources
	}
	return resources, err
}

// Once the resourcemappingapi supports ASGs then this workaround method can be deleted
// https://docs.aws.amazon.com/sdk-for-go/api/service/resourcegroupstaggingapi/
func (iface tagsInterface) getTaggedAutoscalingGroups(job discovery, region string) (resources []*tagsData, err error) {
	ctx := context.Background()
	pageNum := 0
	return resources, iface.asgClient.DescribeAutoScalingGroupsPagesWithContext(ctx, &autoscaling.DescribeAutoScalingGroupsInput{},
		func(page *autoscaling.DescribeAutoScalingGroupsOutput, more bool) bool {
			pageNum++
			autoScalingAPICounter.Inc()

			for _, asg := range page.AutoScalingGroups {
				resource := tagsData{}

				// Transform the ASG ARN into something which looks more like an ARN from the ResourceGroupTaggingAPI
				parts := strings.Split(*asg.AutoScalingGroupARN, ":")
				resource.ID = aws.String(fmt.Sprintf("arn:%s:autoscaling:%s:%s:%s", parts[1], parts[3], parts[4], parts[7]))

				resource.Namespace = &job.Namespace
				resource.Region = &region

				for _, t := range asg.Tags {
					resource.Tags = append(resource.Tags, &tag{Key: *t.Key, Value: *t.Value})
				}

				if resource.filterThroughTags(job.FilterTags) {
					resources = append(resources, &resource)
				}
			}
			return pageNum < 100
		})
}

// Get all ApiGateways REST
func (iface tagsInterface) getTaggedApiGateway() (*apigateway.GetRestApisOutput, error) {
	ctx := context.Background()
	apiGatewayAPICounter.Inc()
	var limit int64 = 500 // max number of results per page. default=25, max=500
	const maxPages = 10
	input := apigateway.GetRestApisInput{Limit: &limit}
	output := apigateway.GetRestApisOutput{}
	var pageNum int
	err := iface.apiGatewayClient.GetRestApisPagesWithContext(ctx, &input, func(page *apigateway.GetRestApisOutput, lastPage bool) bool {
		pageNum++
		output.Items = append(output.Items, page.Items...)
		return pageNum <= maxPages
	})
	return &output, err
}

func (iface tagsInterface) getTaggedEC2SpotInstances(job discovery, region string) (resources []*tagsData, err error) {
	ctx := context.Background()
	pageNum := 0
	return resources, iface.ec2Client.DescribeSpotFleetRequestsPagesWithContext(ctx, &ec2.DescribeSpotFleetRequestsInput{},
		func(page *ec2.DescribeSpotFleetRequestsOutput, more bool) bool {
			pageNum++
			ec2APICounter.Inc()

			for _, ec2Spot := range page.SpotFleetRequestConfigs {
				resource := tagsData{}

				resource.ID = aws.String(fmt.Sprintf("%s", *ec2Spot.SpotFleetRequestId))

				resource.Namespace = &job.Namespace
				resource.Region = &region

				for _, t := range ec2Spot.Tags {
					resource.Tags = append(resource.Tags, &tag{Key: *t.Key, Value: *t.Value})
				}

				if resource.filterThroughTags(job.FilterTags) {
					resources = append(resources, &resource)
				}
			}
			return pageNum < 100
		})
}

func migrateTagsToPrometheus(tagData []*tagsData) []*PrometheusMetric {
	output := make([]*PrometheusMetric, 0)

	tagList := make(map[string][]string)

	for _, d := range tagData {
		for _, entry := range d.Tags {
			found := false
			for _, tag := range tagList[*d.Namespace] {
				if tag == entry.Key {
					found = true
					break
				}
			}
			if !found {
				tagList[*d.Namespace] = append(tagList[*d.Namespace], entry.Key)
			}
		}
	}

	for _, d := range tagData {
		parts := strings.Split(*d.Namespace, "/")
		service := parts[len(parts)-1]
		name := "aws_" + promString(service) + "_info"
		promLabels := make(map[string]string)
		if d.ID != nil {
			promLabels["name"] = *d.ID
		}
		for _, entry := range tagList[*d.Namespace] {
			labelKey := "tag_" + promStringTag(entry)
			promLabels[labelKey] = ""

			for _, rTag := range d.Tags {
				if entry == rTag.Key {
					promLabels[labelKey] = rTag.Value
				}
			}
		}

		var i int
		f := float64(i)

		p := PrometheusMetric{
			name:   &name,
			labels: promLabels,
			value:  &f,
		}

		output = append(output, &p)
	}

	return output
}
