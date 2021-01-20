package main

import (
	"fmt"
	"github.com/go-playground/validator/v10"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"reflect"
	"strings"
)

type conf struct {
	Global    global      `yaml:"global"`
	Discovery []discovery `yaml:"discovery" validate:"required_without=Static,dive,required"`
	Static    []static    `yaml:"static" validate:"required_without=Discovery,dive,required"`
}

type global struct {
	ExportedTagsOnMetrics exportedTagsOnMetrics `yaml:"exportedTagsOnMetrics"`
}

type exportedTagsOnMetrics map[string][]string

type job struct {
	Regions       []string `yaml:"regions" validate:"required"`
	RoleArns      []string `yaml:"roleArns"`
	CustomTags    []tag    `yaml:"customTags" validate:"omitempty,dive,required"`
	Metrics       []metric `yaml:"metrics" validate:"required,dive,required"`
	Namespace     string   `yaml:"namespace" validate:"required,isAwsNamespace"`
	metricsCommon `yaml:",inline"`
}

type discovery struct {
	FilterTags []tag `yaml:"filterTags" validate:"omitempty,dive,required"`
	job        `yaml:",inline"`
}

type static struct {
	Name       string      `yaml:"name" validate:"required"`
	Dimensions []dimension `yaml:"dimensions" validate:"omitempty,dive,required"`
	job        `yaml:",inline"`
}

type metricsCommon struct {
	Period                 int   `yaml:"period" validate:"omitempty,gte=1"`
	Length                 int   `yaml:"length" validate:"omitempty,gtefield=Period"`
	Delay                  int   `yaml:"delay"`
	AddCloudwatchTimestamp bool  `yaml:"addCloudwatchTimestamp"`
	NilToZero              *bool `yaml:"nilToZero"`
}

type metric struct {
	Name          string   `yaml:"name" validate:"required"`
	Statistics    []string `yaml:"statistics" validate:"required"`
	metricsCommon `yaml:",inline"`
}

type dimension struct {
	Name  string `yaml:"name" validate:"required"`
	Value string `yaml:"value" validate:"required"`
}

type tag struct {
	Key   string `yaml:"Key" validate:"required"`
	Value string `yaml:"Value" validate:"required"`
}

var validate *validator.Validate

func (c *conf) load(file *string) error {
	validate = validator.New()
	validate.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := strings.SplitN(fld.Tag.Get("yaml"), ",", 2)[0]
		if name == "-" {
			return ""
		}
		return name
	})
	validate.RegisterValidation("isAwsNamespace", validateAWSNamespace)
	yamlFile, err := ioutil.ReadFile(*file)
	if err != nil {
		return err
	}
	err = yaml.Unmarshal(yamlFile, c)
	if err != nil {
		return err
	}

	if err = validate.Struct(*c); err != nil {
		var errMessage string
		for _, err := range err.(validator.ValidationErrors) {
			if err.Tag() == "required" {
				errMessage = fmt.Sprintf("%v should not be empty", err.Namespace())
			} else if err.Tag() == "isAwsNamespace" {
				errMessage = fmt.Sprintf("%v: Namespace %v is not in a known list", err.Value(), err.Namespace())
			} else if err.Tag() == "gtefield" || err.Tag() == "gtefield" {
				errMessage = fmt.Sprintf("%v: %v(%d) should be greater than %v", err.Namespace(), err.Field(), err.Value(), err.Param())
			} else if err.Tag() == "required_without" {
				errMessage = fmt.Sprintf("%v: Either %v or %v should be defined", err.Namespace(), err.Field(), strings.ToLower(err.Param()))
			}
			return fmt.Errorf(errMessage)
		}
	}

	for id, job := range c.Discovery {
		if c.Discovery[id].job, err = validateJob(job.job); err != nil {
			return err
		}
	}

	for id, job := range c.Static {
		if c.Static[id].job, err = validateJob(job.job); err != nil {
			return err
		}
	}

	return nil
}

func validateAWSNamespace(fl validator.FieldLevel) bool {
	_, ok := supportedNamespaces[fl.Field().String()]
	return ok
}

func validateJob(j job) (job, error) {
	if len(j.RoleArns) == 0 {
		j.RoleArns = []string{""} // use current IAM role
	}
	for idx, metric := range j.Metrics {
		if metric.Period == 0 {
			if j.Period != 0 {
				j.Metrics[idx].Period = j.Period
			} else {
				j.Metrics[idx].Period = 60
			}
		}

		if metric.Length == 0 {
			if j.Length != 0 {
				j.Metrics[idx].Length = j.Length
			} else {
				j.Metrics[idx].Length = 120
			}
		}

		if metric.NilToZero == nil {
			if j.NilToZero != nil {
				j.Metrics[idx].NilToZero = j.NilToZero
			} else {
				j.Metrics[idx].NilToZero = nil
			}
		}

		if j.Metrics[idx].Length < j.Metrics[idx].Period {
			return j, fmt.Errorf("Metric %v in %v: length(%d) is smaller than period(%d)", metric.Name, j.Namespace, j.Metrics[idx].Length, j.Metrics[idx].Period)
		}
	}
	return j, nil
}
