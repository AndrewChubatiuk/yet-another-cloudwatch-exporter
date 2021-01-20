package main

import (
	"fmt"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"github.com/aws/aws-sdk-go/service/sts"
	log "github.com/sirupsen/logrus"
	"math"
	"math/rand"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	cloudwatchSemaphore chan struct{}
	tagSemaphore        chan struct{}
)

func scrapeAwsData(config conf, now time.Time) ([]*tagsData, []*cloudwatchData, *time.Time) {
	mux := &sync.Mutex{}
	cwData := make([]*cloudwatchData, 0)
	awsInfoData := make([]*tagsData, 0)
	var endtime time.Time
	var wg sync.WaitGroup

	for _, discoveryJob := range config.Discovery {
		for _, roleArn := range discoveryJob.RoleArns {
			clientSts := createStsSession(roleArn)
			input := &sts.GetCallerIdentityInput{}
			result, _ := clientSts.GetCallerIdentity(input)
			accountId := result.Account
			for _, region := range discoveryJob.Regions {
				wg.Add(1)
				go func(discoveryJob discovery, region string, roleArn string) {
					defer wg.Done()
					clientCloudwatch := cloudwatchInterface{
						client: createCloudwatchSession(&region, roleArn),
					}
					clientTag := tagsInterface{
						client:           createTagSession(&region, roleArn),
						apiGatewayClient: createAPIGatewaySession(&region, roleArn),
						asgClient:        createASGSession(&region, roleArn),
						ec2Client:        createEC2Session(&region, roleArn),
					}
					var resources []*tagsData
					var metrics []*cloudwatchData
					resources, metrics, endtime = scrapeDiscoveryJobUsingMetricData(discoveryJob, region, accountId, config.Global.ExportedTagsOnMetrics, clientTag, clientCloudwatch, now)
					mux.Lock()
					awsInfoData = append(awsInfoData, resources...)
					cwData = append(cwData, metrics...)
					mux.Unlock()
				}(discoveryJob, region, roleArn)
			}
		}
	}

	for _, staticJob := range config.Static {
		for _, roleArn := range staticJob.RoleArns {
			clientSts := createStsSession(roleArn)
			input := &sts.GetCallerIdentityInput{}
			result, _ := clientSts.GetCallerIdentity(input)
			accountId := result.Account
			for _, region := range staticJob.Regions {
				wg.Add(1)

				go func(staticJob static, region string, roleArn string) {
					clientCloudwatch := cloudwatchInterface{
						client: createCloudwatchSession(&region, roleArn),
					}

					metrics := scrapeStaticJob(staticJob, region, accountId, clientCloudwatch)

					mux.Lock()
					cwData = append(cwData, metrics...)
					mux.Unlock()

					wg.Done()
				}(staticJob, region, roleArn)
			}
		}
	}
	wg.Wait()
	return awsInfoData, cwData, &endtime
}

func scrapeStaticJob(resource static, region string, accountId *string, clientCloudwatch cloudwatchInterface) (cw []*cloudwatchData) {
	mux := &sync.Mutex{}
	var wg sync.WaitGroup

	for j := range resource.Metrics {
		metric := resource.Metrics[j]
		wg.Add(1)
		go func() {
			defer wg.Done()

			cloudwatchSemaphore <- struct{}{}
			defer func() {
				<-cloudwatchSemaphore
			}()

			id := resource.Name
			parts := strings.Split(resource.Namespace, "/")
			service := parts[len(parts)-1]
			data := cloudwatchData{
				ID:                     &id,
				Metric:                 &metric.Name,
				Service:                &service,
				Statistics:             metric.Statistics,
				NilToZero:              metric.NilToZero,
				AddCloudwatchTimestamp: &metric.AddCloudwatchTimestamp,
				CustomTags:             resource.CustomTags,
				Dimensions:             createStaticDimensions(resource.Dimensions),
				Region:                 &region,
				Period:                 int64(metric.Period),
				Delay:                  metric.Delay,
				Length:                 metric.Length,
				AccountID:              accountId,
			}

			filter := createGetMetricStatisticsInput(
				data.Dimensions,
				&resource.Namespace,
				metric,
			)

			data.Points = clientCloudwatch.get(filter)

			if data.Points != nil {
				mux.Lock()
				cw = append(cw, &data)
				mux.Unlock()
			}
		}()
	}
	wg.Wait()
	return cw
}

func getMetricDataForQueries(
	discoveryJob discovery,
	region string,
	accountId *string,
	tagsOnMetrics exportedTagsOnMetrics,
	clientCloudwatch cloudwatchInterface,
	resources []*tagsData) []cloudwatchData {
	var getMetricDatas []cloudwatchData
	namespace := discoveryJob.Namespace
	// For every metric of the job
	for _, metricConfig := range discoveryJob.Metrics {
		tagSemaphore <- struct{}{}
		cloudwatchOutputs := clientCloudwatch.getMetricsByName(namespace, metricConfig)
		<-tagSemaphore
		dimensions := getResourcesDimensions(namespace, resources)
		for _, metric := range cloudwatchOutputs.Metrics {
			if !dimensionsIntersect(metric.Dimensions, dimensions) {
				for _, stats := range metricConfig.Statistics {
					id := fmt.Sprintf("id_%d", rand.Int())
					tags := []*tag{}
					getMetricDatas = append(getMetricDatas, cloudwatchData{
						MetricID:               &id,
						Metric:                 metric.MetricName,
						Service:                metric.Namespace,
						Statistics:             []string{stats},
						NilToZero:              metricConfig.NilToZero,
						AddCloudwatchTimestamp: &metricConfig.AddCloudwatchTimestamp,
						Tags:                   tags,
						CustomTags:             discoveryJob.CustomTags,
						Dimensions:             metric.Dimensions,
						Region:                 &region,
						Period:                 int64(metricConfig.Period),
						Delay:                  metricConfig.Delay,
						Length:                 metricConfig.Length,
						AccountID:              accountId,
					})
				}
				continue
			}
			for _, dimension := range metric.Dimensions {
				if values, ok := dimensions[*dimension.Name]; ok {
					if tags, ok := values[*dimension.Value]; ok {
						metricTags := metricTags(tags, tagsOnMetrics[namespace])
						for _, stats := range metricConfig.Statistics {
							id := fmt.Sprintf("id_%d", rand.Int())
							getMetricDatas = append(getMetricDatas, cloudwatchData{
								ID:                     dimension.Value,
								MetricID:               &id,
								Metric:                 metric.MetricName,
								Service:                metric.Namespace,
								Statistics:             []string{stats},
								NilToZero:              metricConfig.NilToZero,
								AddCloudwatchTimestamp: &metricConfig.AddCloudwatchTimestamp,
								Tags:                   metricTags,
								CustomTags:             discoveryJob.CustomTags,
								Dimensions:             metric.Dimensions,
								Region:                 &region,
								Period:                 int64(metricConfig.Period),
								Delay:                  metricConfig.Delay,
								Length:                 metricConfig.Length,
								AccountID:              accountId,
							})
						}
						break
					}
				}
			}
		}
	}
	return getMetricDatas
}

func dimensionsIntersect(dimensions []*cloudwatch.Dimension, selector map[string]map[string][]*tag) bool {
	for _, d := range dimensions {
		if _, exists := selector[*d.Name]; exists {
			return true
		}
	}
	return false
}

func scrapeDiscoveryJobUsingMetricData(
	job discovery,
	region string,
	accountId *string,
	tagsOnMetrics exportedTagsOnMetrics,
	clientTag tagsInterface,
	clientCloudwatch cloudwatchInterface, now time.Time) (resources []*tagsData, cw []*cloudwatchData, endtime time.Time) {

	namespace := job.Namespace
	// Add the info tags of all the resources
	tagSemaphore <- struct{}{}
	resources, err := clientTag.get(job, region)
	<-tagSemaphore
	if err != nil {
		log.Printf("Couldn't describe resources for region %s: %s\n", region, err.Error())
		return
	}

	getMetricDatas := getMetricDataForQueries(job, region, accountId, tagsOnMetrics, clientCloudwatch, resources)
	maxMetricCount := *metricsPerQuery
	metricDataLength := len(getMetricDatas)
	partition := int(math.Ceil(float64(metricDataLength) / float64(maxMetricCount)))

	mux := &sync.Mutex{}
	var wg sync.WaitGroup
	wg.Add(partition)
	for i := 0; i < metricDataLength; i += maxMetricCount {
		go func(i int) {
			defer wg.Done()
			end := i + maxMetricCount
			if end > metricDataLength {
				end = metricDataLength
			}
			filter := createGetMetricDataInput(getMetricDatas[i:end], &namespace, now)
			data := clientCloudwatch.getMetricData(filter)
			if data != nil {
				for _, MetricDataResult := range data.MetricDataResults {
					getMetricData, err := findGetMetricDataById(getMetricDatas[i:end], *MetricDataResult.Id)
					if err == nil {
						if len(MetricDataResult.Values) != 0 {
							getMetricData.GetMetricDataPoint = MetricDataResult.Values[0]
							getMetricData.GetMetricDataTimestamps = MetricDataResult.Timestamps[0]
						}
						mux.Lock()
						cw = append(cw, &getMetricData)
						mux.Unlock()
					}
				}
			}
			endtime = *filter.EndTime
		}(i)
	}
	//here set end time as start time
	wg.Wait()
	return resources, cw, endtime
}

func (r tagsData) filterThroughTags(filterTags []tag) bool {
	tagMatches := 0

	for _, resourceTag := range r.Tags {
		for _, filterTag := range filterTags {
			if resourceTag.Key == filterTag.Key {
				r, _ := regexp.Compile(filterTag.Value)
				if r.MatchString(resourceTag.Value) {
					tagMatches++
				}
			}
		}
	}

	return tagMatches == len(filterTags)
}

func metricTags(input []*tag, tagsOnMetrics []string) []*tag {
	tags := make([]*tag, 0)
	for _, tagName := range tagsOnMetrics {
		tag := &tag{
			Key: tagName,
		}
		for _, resourceTag := range input {
			if resourceTag.Key == tagName {
				tag.Value = resourceTag.Value
				break
			}
		}

		// Always add the tag, even if it's empty, to ensure the same labels are present on all metrics for a single service
		tags = append(tags, tag)
	}
	return tags
}
