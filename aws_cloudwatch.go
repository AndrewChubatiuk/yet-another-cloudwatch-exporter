package main

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials/stscreds"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"github.com/aws/aws-sdk-go/service/cloudwatch/cloudwatchiface"

	log "github.com/sirupsen/logrus"
)

var percentile = regexp.MustCompile(`^p(\d{1,2}(\.\d{0,2})?|100)$`)

type cloudwatchInterface struct {
	client cloudwatchiface.CloudWatchAPI
}

type cloudwatchData struct {
	ID                      *string
	MetricID                *string
	Metric                  *string
	Service                 *string
	Statistics              []string
	Points                  []*cloudwatch.Datapoint
	GetMetricDataPoint      *float64
	GetMetricDataTimestamps *time.Time
	NilToZero               *bool
	AddCloudwatchTimestamp  *bool
	CustomTags              []tag
	Tags                    []*tag
	Dimensions              []*cloudwatch.Dimension
	Region                  *string
	Period                  int64
	EndTime                 time.Time
	Delay                   int
	Length                  int
}

var labelMap = make(map[string][]string)

func createCloudwatchSession(region *string, roleArn string) *cloudwatch.CloudWatch {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))

	maxCloudwatchRetries := 5

	config := &aws.Config{Region: region, MaxRetries: &maxCloudwatchRetries}

	if *fips {
		// https://docs.aws.amazon.com/general/latest/gr/cw_region.html
		endpoint := fmt.Sprintf("https://monitoring-fips.%s.amazonaws.com", *region)
		config.Endpoint = aws.String(endpoint)
	}

	if *debug {
		config.LogLevel = aws.LogLevel(aws.LogDebugWithHTTPBody)
	}

	if roleArn != "" {
		config.Credentials = stscreds.NewCredentials(sess, roleArn)
	}

	return cloudwatch.New(sess, config)
}

func createGetMetricStatisticsInput(dimensions []*cloudwatch.Dimension, namespace *string, metric metric) (output *cloudwatch.GetMetricStatisticsInput) {
	period := int64(metric.Period)
	length := metric.Length
	delay := metric.Delay
	endTime := time.Now().Add(-time.Duration(delay) * time.Second)
	startTime := time.Now().Add(-(time.Duration(length) + time.Duration(delay)) * time.Second)

	var statistics []*string
	var extendedStatistics []*string
	for _, statistic := range metric.Statistics {
		if percentile.MatchString(statistic) {
			extendedStatistics = append(extendedStatistics, aws.String(statistic))
		} else {
			statistics = append(statistics, aws.String(statistic))
		}
	}

	output = &cloudwatch.GetMetricStatisticsInput{
		Dimensions:         dimensions,
		Namespace:          namespace,
		StartTime:          &startTime,
		EndTime:            &endTime,
		Period:             &period,
		MetricName:         &metric.Name,
		Statistics:         statistics,
		ExtendedStatistics: extendedStatistics,
	}

	if len(statistics) != 0 {
		log.Debug("CLI helper - " +
			"aws cloudwatch get-metric-statistics" +
			" --metric-name " + metric.Name +
			" --dimensions " + dimensionsToCliString(dimensions) +
			" --namespace " + *namespace +
			" --statistics " + *statistics[0] +
			" --period " + strconv.FormatInt(period, 10) +
			" --start-time " + startTime.Format(time.RFC3339) +
			" --end-time " + endTime.Format(time.RFC3339))
	}
	log.Debug(*output)
	return output
}

func findGetMetricDataById(getMetricDatas []cloudwatchData, value string) (cloudwatchData, error) {
	var g cloudwatchData
	for _, getMetricData := range getMetricDatas {
		if *getMetricData.MetricID == value {
			return getMetricData, nil
		}
	}
	return g, fmt.Errorf("Metric with id %s not found", value)
}

func createGetMetricDataInput(getMetricData []cloudwatchData, namespace *string, now time.Time) (output *cloudwatch.GetMetricDataInput) {
	var metricsDataQuery []*cloudwatch.MetricDataQuery
	var delay int
	var length int
	for _, data := range getMetricData {
		metricStat := &cloudwatch.MetricStat{
			Metric: &cloudwatch.Metric{
				Dimensions: data.Dimensions,
				MetricName: data.Metric,
				Namespace:  namespace,
			},
			Period: &data.Period,
			Stat:   &data.Statistics[0],
		}
		ReturnData := true
		metricsDataQuery = append(metricsDataQuery, &cloudwatch.MetricDataQuery{
			Id:         data.MetricID,
			MetricStat: metricStat,
			ReturnData: &ReturnData,
		})
		length = data.Length
		delay = data.Delay
	}

	var endTime time.Time
	var startTime time.Time
	if now.IsZero() {
		//This is first run
		now = time.Now().Round(5 * time.Minute)
		endTime = now.Add(-time.Duration(delay) * time.Second)
		startTime = now.Add(-(time.Duration(length) + time.Duration(delay)) * time.Second)
	} else {
		endTime = now.Add(time.Duration(length) * time.Second)
		startTime = now
	}

	dataPointOrder := "TimestampDescending"
	output = &cloudwatch.GetMetricDataInput{
		EndTime:           &endTime,
		StartTime:         &startTime,
		MetricDataQueries: metricsDataQuery,
		ScanBy:            &dataPointOrder,
	}

	return output
}

func dimensionsToCliString(dimensions []*cloudwatch.Dimension) (output string) {
	for _, dim := range dimensions {
		output = output + "Name=" + *dim.Name + ",Value=" + *dim.Value
	}
	return output
}

func (iface cloudwatchInterface) get(filter *cloudwatch.GetMetricStatisticsInput) []*cloudwatch.Datapoint {
	c := iface.client

	log.Debug(filter)

	resp, err := c.GetMetricStatistics(filter)

	log.Debug(resp)

	cloudwatchAPICounter.Inc()
	cloudwatchGetMetricStatisticsAPICounter.Inc()

	if err != nil {
		log.Warningf("Unable to get metric statistics due to %v", err)
		return nil
	}

	return resp.Datapoints
}

func (iface cloudwatchInterface) getMetricData(filter *cloudwatch.GetMetricDataInput) *cloudwatch.GetMetricDataOutput {
	c := iface.client

	var resp cloudwatch.GetMetricDataOutput

	if *debug {
		log.Println(filter)
	}

	// Using the paged version of the function
	err := c.GetMetricDataPages(filter,
		func(page *cloudwatch.GetMetricDataOutput, lastPage bool) bool {
			cloudwatchAPICounter.Inc()
			cloudwatchGetMetricDataAPICounter.Inc()
			resp.MetricDataResults = append(resp.MetricDataResults, page.MetricDataResults...)
			return !lastPage
		})

	if *debug {
		log.Println(resp)
	}

	if err != nil {
		log.Warningf("Unable to get metric data due to %v", err)
		return nil
	}
	return &resp
}

func createStaticDimensions(dimensions []dimension) (output []*cloudwatch.Dimension) {
	for i, _ := range dimensions {
		output = append(output, &cloudwatch.Dimension{
			Name:  &dimensions[i].Name,
			Value: &dimensions[i].Value,
		})
	}
	return output
}

func (c *cloudwatchInterface) getMetricsByName(namespace string, metric metric) (resp *cloudwatch.ListMetricsOutput) {
	input := &cloudwatch.ListMetricsInput{
		MetricName: &metric.Name,
		Namespace:  &namespace,
		NextToken:  nil,
	}
	var res cloudwatch.ListMetricsOutput
	err := c.client.ListMetricsPages(input,
		func(page *cloudwatch.ListMetricsOutput, lastPage bool) bool {
			res.Metrics = append(res.Metrics, page.Metrics...)
			return !lastPage
		})
	cloudwatchAPICounter.Inc()
	if err != nil {
		log.Fatalf("Unable to list metrics due to %v", err)
	}
	return &res
}

func (r *tagsData) getDimensions(metrics []*cloudwatch.Metric) (dimensions [][]*cloudwatch.Dimension) {
	detectedDimensions := make(map[string]string)
	if params, ok := supportedNamespaces[*r.Namespace]; ok {
		if r.ID != nil {
			for _, d := range params.Dimensions {
				regex := regexp.MustCompile(d)
				if regex.Match([]byte(*r.ID)) {
					match := regex.FindStringSubmatch(*r.ID)
					for i, value := range match {
						if regex.SubexpNames()[i] != "" {
							detectedDimensions[regex.SubexpNames()[i]] = value
						}
					}
				}
			}
		}
		if len(detectedDimensions) > 0 || r.ID == nil {
			for _, metric := range metrics {
				if r.ID == nil {
					dimensions = append(dimensions, metric.Dimensions)
				} else {
					for _, dimension := range metric.Dimensions {
						value, exists := detectedDimensions[*dimension.Name]
						if exists && value == *dimension.Value {
							dimensions = append(dimensions, metric.Dimensions)
						}
					}
				}
			}
		}

	}
	return dimensions
}

func (c *cloudwatchData) createPrometheusLabels() map[string]string {
	labels := make(map[string]string)
	if len(c.Dimensions) == 0 && c.ID != nil {
		labels["name"] = *c.ID
	}
	labels["region"] = *c.Region
	for _, dimension := range c.Dimensions {
		labels["dimension_"+promStringTag(*dimension.Name)] = *dimension.Value
	}
	for _, label := range c.CustomTags {
		labels["custom_tag_"+promStringTag(label.Key)] = label.Value
	}
	for _, tag := range c.Tags {
		labels["tag_"+promStringTag(tag.Key)] = tag.Value
	}
	return labels
}

func recordLabelsForMetric(metricName string, promLabels map[string]string) {
	var workingLabelsCopy []string
	if _, ok := labelMap[metricName]; ok {
		workingLabelsCopy = append(workingLabelsCopy, labelMap[metricName]...)
	}

	for k, _ := range promLabels {
		workingLabelsCopy = append(workingLabelsCopy, k)
	}
	sort.Strings(workingLabelsCopy)
	j := 0
	for i := 1; i < len(workingLabelsCopy); i++ {
		if workingLabelsCopy[j] == workingLabelsCopy[i] {
			continue
		}
		j++
		workingLabelsCopy[j] = workingLabelsCopy[i]
	}
	labelMap[metricName] = workingLabelsCopy[:j+1]
}

func ensureLabelConsistencyForMetrics(metrics []*PrometheusMetric) (output []*PrometheusMetric) {
	for _, prometheusMetric := range metrics {
		metricName := prometheusMetric.name
		metricLabels := prometheusMetric.labels
		consistentMetricLabels := make(map[string]string)
		for _, recordedLabel := range labelMap[*metricName] {
			if value, ok := metricLabels[recordedLabel]; ok {
				consistentMetricLabels[recordedLabel] = value
			} else {
				consistentMetricLabels[recordedLabel] = ""
			}
		}
		prometheusMetric.labels = consistentMetricLabels
		output = append(output, prometheusMetric)
	}
	return output
}

func sortByTimestamp(datapoints []*cloudwatch.Datapoint) []*cloudwatch.Datapoint {
	sort.Slice(datapoints, func(i, j int) bool {
		jTimestamp := *datapoints[j].Timestamp
		return datapoints[i].Timestamp.After(jTimestamp)
	})
	return datapoints
}

func (c *cloudwatchData) getDatapoint(statistic string) (*float64, time.Time) {
	if c.GetMetricDataPoint != nil {
		return c.GetMetricDataPoint, *c.GetMetricDataTimestamps
	}
	var averageDataPoints []*cloudwatch.Datapoint

	// sorting by timestamps so we can consistently export the most updated datapoint
	// assuming Timestamp field in cloudwatch.Datapoint struct is never nil
	for _, datapoint := range sortByTimestamp(c.Points) {
		switch {
		case statistic == "Maximum":
			if datapoint.Maximum != nil {
				return datapoint.Maximum, *datapoint.Timestamp
			}
		case statistic == "Minimum":
			if datapoint.Minimum != nil {
				return datapoint.Minimum, *datapoint.Timestamp
			}
		case statistic == "Sum":
			if datapoint.Sum != nil {
				return datapoint.Sum, *datapoint.Timestamp
			}
		case statistic == "SampleCount":
			if datapoint.SampleCount != nil {
				return datapoint.SampleCount, *datapoint.Timestamp
			}
		case statistic == "Average":
			if datapoint.Average != nil {
				averageDataPoints = append(averageDataPoints, datapoint)
			}
		case percentile.MatchString(statistic):
			if data, ok := datapoint.ExtendedStatistics[statistic]; ok {
				return data, *datapoint.Timestamp
			}
		default:
			log.Fatal("Not implemented statistics: " + statistic)
		}
	}

	if len(averageDataPoints) > 0 {
		var total float64
		var timestamp time.Time

		for _, p := range averageDataPoints {
			if p.Timestamp.After(timestamp) {
				timestamp = *p.Timestamp
			}
			total += *p.Average
		}
		average := total / float64(len(averageDataPoints))
		return &average, timestamp
	}
	return nil, time.Time{}
}

func migrateCloudwatchToPrometheus(cwd []*cloudwatchData) []*PrometheusMetric {
	output := make([]*PrometheusMetric, 0)

	for _, c := range cwd {
		for _, statistic := range c.Statistics {
			includeTimestamp := *c.AddCloudwatchTimestamp
			exportedDatapoint, timestamp := c.getDatapoint(statistic)
			if exportedDatapoint == nil {
				var nan float64 = math.NaN()
				exportedDatapoint = &nan
				includeTimestamp = false
				if *c.NilToZero {
					var zero float64 = 0
					exportedDatapoint = &zero
				}
			}
			reg := regexp.MustCompile(`.*/([^ ]*)`)
			res := reg.ReplaceAllString(*c.Service, "${1}")
			name := "aws_" + strings.ToLower(res) + "_" + strings.ToLower(promString(*c.Metric)) + "_" + strings.ToLower(promString(statistic))
			if exportedDatapoint != nil {
				promLabels := c.createPrometheusLabels()
				recordLabelsForMetric(name, promLabels)
				p := PrometheusMetric{
					name:             &name,
					labels:           promLabels,
					value:            exportedDatapoint,
					timestamp:        timestamp,
					includeTimestamp: includeTimestamp,
				}
				output = append(output, &p)
			}
		}
	}

	return output
}
