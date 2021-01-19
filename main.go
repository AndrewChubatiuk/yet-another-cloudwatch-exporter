package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	log "github.com/sirupsen/logrus"
)

var version = "custom-build"

type namespaces map[string]resource
type PromStats struct {
	Registry            *prometheus.Registry
	ProcessingTimeTotal time.Duration
	Now                 time.Time
}

type resource struct {
	Resources     []string
	Dimensions    []string
	GlobalMetrics bool
}

var (
	addr                  = flag.String("listen-address", ":5000", "The address to listen on.")
	configFile            = flag.String("config.file", "config.yml", "Path to configuration file.")
	debug                 = flag.Bool("debug", false, "Add verbose logging.")
	fips                  = flag.Bool("fips", false, "Use FIPS compliant aws api.")
	showVersion           = flag.Bool("v", false, "prints current yace version.")
	cloudwatchConcurrency = flag.Int("cloudwatch-concurrency", 5, "Maximum number of concurrent requests to CloudWatch API.")
	tagConcurrency        = flag.Int("tag-concurrency", 5, "Maximum number of concurrent requests to Resource Tagging API.")
	scrapingInterval      = flag.Int("scraping-interval", 300, "Seconds to wait between scraping the AWS metrics if decoupled scraping.")
	decoupledScraping     = flag.Bool("decoupled-scraping", true, "Decouples scraping and serving of metrics.")
	metricsPerQuery       = flag.Int("metrics-per-query", 500, "Number of metrics made in a single GetMetricsData request")
	labelsSnakeCase       = flag.Bool("labels-snake-case", false, "If labels should be output in snake case instead of camel case")
	promStats             = PromStats{
		Registry: prometheus.NewRegistry(),
	}

	supportedNamespaces = namespaces{
		"AWS/ApplicationELB": {
			Resources: []string{"elasticloadbalancing:loadbalancer/app", "elasticloadbalancing:targetgroup"},
			Dimensions: []string{
				":(?P<TargetGroup>targetgroup/.+)",
				":loadbalancer/(?P<LoadBalancer>.+)$",
			},
		},
		"AWS/ApiGateway": {
			Resources: []string{"apigateway"},
			Dimensions: []string{
				"apis/(?P<ApiName>[^/]+)",
				"stages/(?P<Stage>[^/]+)",
			},
		},
		"AWS/AppSync": {
			Resources: []string{"appsync"},
			Dimensions: []string{
				"apis/(?P<GraphQLAPIId>[^/]+)",
			},
		},
		"AWS/AutoScaling": {
			Dimensions: []string{
				"autoScalingGroupName/(?P<AutoScalingGroupName>[^/]+)",
			},
		},
		"AWS/Billing": {
			GlobalMetrics: true,
		},
		"AWS/CloudFront": {
			Resources: []string{"cloudfront:distribution"},
			Dimensions: []string{
				"distribution/(?P<DistributionId>[^/]+)",
			},
		},
		"AWS/Cognito": {
			Resources: []string{"cognito-idp:userpool"},
			Dimensions: []string{
				"userpool/(?P<UserPool>[^/]+)",
			},
		},
		"AWS/DocDB": {
			Resources: []string{"rds:db", "rds:cluster"},
			Dimensions: []string{
				"cluster:(?P<DBClusterIdentifier>[^/]+)",
				"db:(?P<DBInstanceIdentifier>[^/]+)",
			},
		},
		"AWS/DynamoDB": {
			Resources: []string{"dynamodb:table"},
			Dimensions: []string{
				":table/(?P<TableName>[^/]+)",
			},
			GlobalMetrics: true,
		},
		"AWS/EBS": {
			Resources: []string{"ec2:volume"},
			Dimensions: []string{
				"volume/(?P<VolumeId>[^/]+)",
			},
		},
		"AWS/ElastiCache": {
			Resources: []string{"elasticache:cluster"},
			Dimensions: []string{
				"cluster:(?P<CacheClusterId>[^/]+)",
			},
		},
		"AWS/EC2": {
			Resources: []string{"ec2:instance"},
			Dimensions: []string{
				"instance/(?P<InstanceId>[^/]+)",
			},
		},
		"AWS/EC2Spot": {
			Resources: []string{"ec2:instance"},
		},
		"AWS/ECS": {
			Resources: []string{"ecs:cluster", "ecs:service"},
			Dimensions: []string{
				"cluster/(?P<ClusterName>[^/]+)",
				"service/(?P<ClusterName>[^/]+)/([^/]+)",
			},
		},
		"ECS/ContainerInsights": {
			Resources: []string{"ecs:cluster", "ecs:service"},
			Dimensions: []string{
				"cluster/(?P<ClusterName>[^/]+)",
				"service/(?P<ClusterName>[^/]+)/([^/]+)",
			},
		},
		"AWS/EFS": {
			Resources: []string{"elasticfilesystem:file-system"},
			Dimensions: []string{
				"file-system/(?P<FileSystemId>[^/]+)",
			},
		},
		"AWS/ELB": {
			Resources: []string{"elasticloadbalancing:loadbalancer"},
			Dimensions: []string{
				":loadbalancer/(?P<LoadBalancer>.+)$",
			},
		},
		"AWS/ElasticMapReduce": {
			Resources: []string{"elasticmapreduce:cluster"},
			Dimensions: []string{
				"cluster/(?P<JobFlowId>[^/]+)",
			},
		},
		"AWS/ES": {
			Resources: []string{"es:domain"},
			Dimensions: []string{
				":domain/(?P<DomainName>[^/]+)",
			},
		},
		"AWS/Firehose": {
			Resources: []string{"firehose"},
			Dimensions: []string{
				":deliverystream/(?P<DeliveryStreamName>[^/]+)",
			},
		},
		"AWS/FSx": {
			Resources: []string{"fsx:file-system"},
			Dimensions: []string{
				"file-system/(?P<FileSystemId>[^/]+)",
			},
		},
		"AWS/GameLift": {
			Resources: []string{"gamelift"},
			Dimensions: []string{
				":fleet/(?P<FleetId>[^/]+)",
			},
		},
		"Glue": {
			Resources: []string{"glue:job"},
			Dimensions: []string{
				":job/(?P<JobName>[^/]+)",
			},
		},
		"AWS/IoT": {
			Resources: []string{},
		},
		"AWS/Kafka": {
			Resources: []string{"kafka:cluster"},
			Dimensions: []string{
				":cluster/(?P<Cluster Name>[^/]+)",
			},
		},
		"AWS/Kinesis": {
			Resources: []string{"kinesis:stream"},
			Dimensions: []string{
				":stream/(?P<StreamName>[^/]+)",
			},
		},
		"AWS/Lambda": {
			Resources: []string{"lambda:function"},
			Dimensions: []string{
				":function:(?P<FunctionName>[^/]+)",
			},
		},
		"AWS/NATGateway": {
			Resources: []string{"ec2:natgateway"},
			Dimensions: []string{
				"natgateway/(?P<NatGatewayId>[^/]+)",
			},
		},
		"AWS/NetworkELB": {
			Resources: []string{"elasticloadbalancing:loadbalancer/net", "elasticloadbalancing:targetgroup"},
			Dimensions: []string{
				":(?P<TargetGroup>targetgroup/.+)",
				":loadbalancer/(?P<LoadBalancer>.+)$",
			},
		},
		"AWS/RDS": {
			Resources: []string{"rds:db", "rds:cluster"},
			Dimensions: []string{
				":cluster:(?P<DBClusterIdentifier>[^/]+)",
				":db:(?P<DBInstanceIdentifier>[^/]+)",
			},
		},
		"AWS/Redshift": {
			Resources: []string{"redshift:cluster"},
			Dimensions: []string{
				":cluster:(?P<ClusterIdentifier>[^/]+)",
			},
		},
		"AWS/Route53Resolver": {
			Resources: []string{"route53resolver"},
			Dimensions: []string{
				":resolver-endpoint/(?P<EndpointId>[^/]+)",
			},
		},
		"AWS/S3": {
			Resources: []string{"s3"},
			Dimensions: []string{
				"(?P<BucketName>[^:]+)$",
			},
		},
		"AWS/States": {
			Resources: []string{"states"},
			Dimensions: []string{
				"(?P<StateMachineArn>.*)",
			},
		},
		"AWS/SNS": {
			Resources: []string{"sns"},
			Dimensions: []string{
				"(?P<TopicName>[^:]+)$",
			},
		},
		"AWS/SQS": {
			Resources: []string{"sqs"},
			Dimensions: []string{
				"(?P<QueueName>[^:]+)$",
			},
		},
		"AWS/TransitGateway": {
			Resources: []string{"ec2:transit-gateway"},
			Dimensions: []string{
				":transit-gateway/(?P<TransitGateway>[^/]+)",
			},
		},
		"AWS/VPN": {
			Resources: []string{"ec2:vpn-connection"},
			Dimensions: []string{
				":vpn-connection/(?P<VpnId>[^/]+)",
			},
		},
		"AWS/WAFV2": {
			Resources: []string{"wafv2"},
			Dimensions: []string{
				"/webacl/(?P<WebACL>[^/]+)",
			},
		},
	}
	config = conf{}
)

func init() {

	// Set JSON structured logging as the default log formatter
	log.SetFormatter(&log.JSONFormatter{})

	// Set the Output to stdout instead of the default stderr
	log.SetOutput(os.Stdout)

	// Only log Info severity or above.
	log.SetLevel(log.InfoLevel)

}

func (stats *PromStats) updateMetrics() {
	tagsData, cloudwatchData, now := scrapeAwsData(config, stats.Now)
	stats.Now = *now
	var metrics []*PrometheusMetric

	metrics = append(metrics, migrateCloudwatchToPrometheus(cloudwatchData)...)
	metrics = ensureLabelConsistencyForMetrics(metrics)

	metrics = append(metrics, migrateTagsToPrometheus(tagsData)...)
	registry := prometheus.NewRegistry()
	registry.MustRegister(NewPrometheusCollector(metrics))
	for _, counter := range []prometheus.Counter{cloudwatchAPICounter, cloudwatchGetMetricDataAPICounter, cloudwatchGetMetricStatisticsAPICounter, resourceGroupTaggingAPICounter, autoScalingAPICounter, apiGatewayAPICounter, targetGroupsAPICounter} {
		if err := registry.Register(counter); err != nil {
			log.Warning("Could not publish cloudwatch api metric")
		}
	}
	stats.Registry = registry
}

func main() {
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	if *debug {
		log.SetLevel(log.DebugLevel)
	}

	log.Println("Parse config..")
	if err := config.load(configFile); err != nil {
		log.Fatal("Couldn't read ", *configFile, ": ", err)
	}

	cloudwatchSemaphore = make(chan struct{}, *cloudwatchConcurrency)
	tagSemaphore = make(chan struct{}, *tagConcurrency)
	for _, discoveryJob := range config.Discovery {
		for _, metric := range discoveryJob.Metrics {
			if *scrapingInterval < metric.Length && discoveryJob.Namespace != "AWS/S3" {
				*scrapingInterval = metric.Length
			}
		}
	}
	log.Println("Startup completed")

	if *decoupledScraping {
		go func() {
			for {
				promStats.refreshMetrics(*scrapingInterval)
			}
		}()
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>
		<head><title>Yet another cloudwatch exporter</title></head>
		<body>
		<h1>Thanks for using our product :)</h1>
		<p><a href="/metrics">Metrics</a></p>
		</body>
		</html>`))
	})

	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if !(*decoupledScraping) {
			promStats.refreshMetrics(*scrapingInterval)
		}
		handler := promhttp.HandlerFor(promStats.Registry, promhttp.HandlerOpts{
			DisableCompression: false,
		})
		handler.ServeHTTP(w, r)
	})

	log.Fatal(http.ListenAndServe(*addr, nil))
}

func (stats *PromStats) refreshMetrics(scrapingInterval int) {
	startTime := time.Now()
	stats.updateMetrics()
	endTime := time.Now()
	stats.ProcessingTimeTotal = stats.ProcessingTimeTotal + endTime.Sub(startTime)
	if stats.ProcessingTimeTotal.Seconds() > 60.0 {
		sleepInterval := scrapingInterval - int(stats.ProcessingTimeTotal.Seconds())
		stats.ProcessingTimeTotal = 0
		if sleepInterval <= 0 {
			log.Debug("Unable to sleep since we lagging behind please try adjusting your scrape interval or running this instance with less number of metrics")
			return
		} else {
			log.Debug("Sleeping smaller intervals to catchup with lag", sleepInterval)
			time.Sleep(time.Duration(sleepInterval) * time.Second)
		}

	} else {
		log.Debug("Sleeping at regular sleep interval ", scrapingInterval)
		time.Sleep(time.Duration(scrapingInterval) * time.Second)
	}
}
