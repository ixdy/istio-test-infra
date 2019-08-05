// Copyright 2019 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	"github.com/ghodss/yaml"
	"github.com/hashicorp/go-multierror"
	"github.com/kr/pretty"
	v1 "k8s.io/api/core/v1"
	prowjob "k8s.io/test-infra/prow/apis/prowjobs/v1"
	"k8s.io/test-infra/prow/config"
)

func exit(err error, context string) {
	if context == "" {
		_, _ = fmt.Fprint(os.Stderr, fmt.Sprintf("%v", err))
	} else {
		_, _ = fmt.Fprint(os.Stderr, fmt.Sprintf("%v: %v", context, err))
	}
	os.Exit(1)
}

const (
	TestGridDashboard   = "testgrid-dashboards"
	TestGridAlertEmail  = "testgrid-alert-email"
	TestGridNumFailures = "testgrid-num-failures-to-alert"

	BuilderImage  = "gcr.io/istio-testing/istio-builder:v20190709-959ee177"
	AutogenHeader = "# THIS FILE IS AUTOGENERATED. See prow/config/README.md\n"

	DefaultResource = "default"

	ModifierHidden   = "hidden"
	ModifierOptional = "optional"
	ModifierSkipped  = "skipped"

	TypePostsubmit = "postsubmit"
	TypePresubmit  = "presubmit"

	RequirementRoot = "root"
	RequirementKind = "kind"
	RequirementGCP  = "gcp"
)

type JobConfig struct {
	Jobs      []Job                              `json:"jobs"`
	Repo      string                             `json:"repo"`
	Org       string                             `json:"org"`
	Branches  []string                           `json:"branches"`
	Resources map[string]v1.ResourceRequirements `json:"resources"`
}

type Job struct {
	Name           string            `json:"name"`
	PostsubmitName string            `json:"postsubmit"`
	Command        []string          `json:"command"`
	Resources      string            `json:"resources"`
	Modifiers      []string          `json:"modifiers"`
	Requirements   []string          `json:"requirements"`
	Type           string            `json:"type"`
	Timeout        *prowjob.Duration `json:"timeout"`
}

// Reads the job yaml
func ReadJobConfig(file string) JobConfig {
	yamlFile, err := ioutil.ReadFile(file)
	if err != nil {
		exit(err, "failed to read "+file)
	}
	jobs := JobConfig{}
	if err := yaml.Unmarshal(yamlFile, &jobs); err != nil {
		exit(err, "failed to unmarshal "+file)
	}
	return jobs
}

func ValidateJobConfig(jobConfig JobConfig) {
	var err error
	if _, f := jobConfig.Resources[DefaultResource]; !f {
		err = multierror.Append(err, fmt.Errorf("'%v' resource must be provided", DefaultResource))
	}
	for _, job := range jobConfig.Jobs {
		if job.Resources != "" {
			if _, f := jobConfig.Resources[job.Resources]; !f {
				err = multierror.Append(err, fmt.Errorf("job '%v' has nonexistant resource '%v'", job.Name, job.Resources))
			}
		}
		for _, mod := range job.Modifiers {
			if e := validate(mod, []string{ModifierHidden, ModifierOptional, ModifierSkipped}, "status"); e != nil {
				err = multierror.Append(err, e)
			}
		}
		for _, req := range job.Requirements {
			if e := validate(req, []string{RequirementKind, RequirementRoot, RequirementGCP}, "requirements"); e != nil {
				err = multierror.Append(err, e)
			}
		}
		if e := validate(job.Type, []string{TypePostsubmit, TypePresubmit, ""}, "type"); e != nil {
			err = multierror.Append(err, e)
		}
	}
	if err != nil {
		exit(err, "validation failed")
	}
}

func ConvertJobConfig(jobConfig JobConfig, branch string) config.JobConfig {
	presubmits := []config.Presubmit{}
	postsubmits := []config.Postsubmit{}

	output := config.JobConfig{
		Presubmits:  map[string][]config.Presubmit{},
		Postsubmits: map[string][]config.Postsubmit{},
	}
	for _, job := range jobConfig.Jobs {
		brancher := config.Brancher{
			Branches: []string{fmt.Sprintf("^%s$", branch)},
		}
		// Commands are run with the entrypoint wrapper which will start up prereqs
		// TODO probably not all tests need this
		job.Command = append([]string{"entrypoint"}, job.Command...)

		if job.Type == TypePresubmit || job.Type == "" {
			presubmit := config.Presubmit{
				JobBase:   createJobBase(job, fmt.Sprintf("%s-%s", job.Name, branch), jobConfig.Resources),
				AlwaysRun: true,
				Brancher:  brancher,
			}
			presubmit.JobBase.Annotations[TestGridDashboard] = fmt.Sprintf("%s-presubmits-%s", jobConfig.Repo, branch)
			applyModifiersPresubmit(&presubmit, job.Modifiers)
			applyRequirements(&presubmit.JobBase, job.Requirements)
			presubmits = append(presubmits, presubmit)
		}

		if job.Type == TypePostsubmit || job.Type == "" {
			postName := job.PostsubmitName
			if postName == "" {
				postName = job.Name
			}
			postsubmit := config.Postsubmit{
				JobBase:  createJobBase(job, fmt.Sprintf("%s-%s", postName, branch), jobConfig.Resources),
				Brancher: brancher,
			}
			postsubmit.JobBase.Annotations[TestGridDashboard] = fmt.Sprintf("%s-postsubmits-%s", jobConfig.Repo, branch)
			postsubmit.JobBase.Annotations[TestGridAlertEmail] = "istio-oncall@googlegroups.com"
			postsubmit.JobBase.Annotations[TestGridNumFailures] = "1"
			applyModifiersPostsubmit(&postsubmit, job.Modifiers)
			applyRequirements(&postsubmit.JobBase, job.Requirements)
			postsubmits = append(postsubmits, postsubmit)
		}
		output.Presubmits[fmt.Sprintf("%s/%s", jobConfig.Org, jobConfig.Repo)] = presubmits
		output.Postsubmits[fmt.Sprintf("%s/%s", jobConfig.Org, jobConfig.Repo)] = postsubmits
	}
	return output
}

func CheckConfig(jobs config.JobConfig, currentConfigFile string) error {
	current, err := ioutil.ReadFile(currentConfigFile)
	if err != nil {
		return fmt.Errorf("failed to read current config for %s: %v", currentConfigFile, err)
	}

	newConfig, err := yaml.Marshal(jobs)
	if err != nil {
		return fmt.Errorf("failed to marshal result: %v", err)
	}
	output := []byte(AutogenHeader)
	output = append(output, newConfig...)

	if !bytes.Equal(current, output) {
		return fmt.Errorf("generated config is different than file %v", currentConfigFile)
	}
	return nil
}

func WriteConfig(jobs config.JobConfig, fname string) {
	bytes, err := yaml.Marshal(jobs)
	if err != nil {
		exit(err, "failed to marshal result")
	}
	output := []byte(AutogenHeader)
	output = append(output, bytes...)
	err = ioutil.WriteFile(fname, output, 0755)
	if err != nil {
		exit(err, "failed to write result")
	}
}

func PrintConfig(c interface{}) {
	bytes, err := yaml.Marshal(c)
	if err != nil {
		exit(err, "failed to write result")
	}
	fmt.Println(string(bytes))
}

func validate(input string, options []string, description string) error {
	valid := false
	for _, opt := range options {
		if input == opt {
			valid = true
		}
	}
	if !valid {
		return fmt.Errorf("'%v' is not a valid %v. Must be one of %v", input, description, strings.Join(options, ", "))
	}
	return nil
}

func DiffConfig(result config.JobConfig, existing config.JobConfig) {
	fmt.Println("Presubmit diff:")
	diffConfigPresubmit(result, existing)
	fmt.Println("\n\nPostsubmit diff:")
	diffConfigPostsubmit(result, existing)
}

func diffConfigPresubmit(result config.JobConfig, pj config.JobConfig) {
	known := make(map[string]struct{})
	for _, job := range result.AllPresubmits([]string{"istio/istio"}) {
		known[job.Name] = struct{}{}
		current := pj.GetPresubmit("istio/istio", job.Name)
		if current == nil {
			fmt.Println("\nCreated unknown presubmit job", job.Name)
			continue
		}
		diff := pretty.Diff(current, &job)
		if len(diff) > 0 {
			fmt.Println("\nDiff for", job.Name)
		}
		for _, d := range diff {
			fmt.Println(d)
		}
	}
	for _, job := range pj.AllPresubmits([]string{"istio/istio"}) {
		if _, f := known[job.Name]; !f {
			fmt.Println("Missing", job.Name)
		}
	}
}

func diffConfigPostsubmit(result config.JobConfig, pj config.JobConfig) {
	known := make(map[string]struct{})
	allCurrentPostsubmits := pj.AllPostsubmits([]string{"istio/istio"})
	for _, job := range result.AllPostsubmits([]string{"istio/istio"}) {
		known[job.Name] = struct{}{}
		var current *config.Postsubmit
		for _, ps := range allCurrentPostsubmits {
			if ps.Name == job.Name {
				current = &ps
				break
			}
		}
		if current == nil {
			fmt.Println("\nCreated unknown job:", job.Name)
			continue

		}
		diff := pretty.Diff(current, &job)
		if len(diff) > 0 {
			fmt.Println("\nDiff for", job.Name)
		}
		for _, d := range diff {
			fmt.Println(d)
		}
	}

	for _, job := range pj.AllPostsubmits([]string{"istio/istio"}) {
		if _, f := known[job.Name]; !f {
			fmt.Println("Missing", job.Name)
		}
	}

}

func createContainer(job Job, resources map[string]v1.ResourceRequirements) []v1.Container {
	c := v1.Container{
		Image:           BuilderImage,
		SecurityContext: &v1.SecurityContext{Privileged: newTrue()},
		Command:         job.Command,
	}
	resource := DefaultResource
	if job.Resources != "" {
		resource = job.Resources
	}
	c.Resources = resources[resource]

	return []v1.Container{c}
}

func createJobBase(job Job, name string, resources map[string]v1.ResourceRequirements) config.JobBase {
	jb := config.JobBase{
		Name: name,
		Spec: &v1.PodSpec{
			NodeSelector: map[string]string{"testing": "test-pool"},
			Containers:   createContainer(job, resources),
		},
		UtilityConfig: config.UtilityConfig{
			Decorate: true,

			PathAlias: "istio.io/istio",
		},
		Labels:      make(map[string]string),
		Annotations: make(map[string]string),
	}
	if job.Timeout != nil {
		jb.DecorationConfig = &prowjob.DecorationConfig{
			Timeout: job.Timeout,
		}
	}
	return jb
}

func applyRequirements(job *config.JobBase, requirements []string) {
	for _, req := range requirements {
		switch req {
		case RequirementGCP:
			// GCP resources are limited, so set max concurrency to 5
			// The preset service account will set up the required resources
			job.MaxConcurrency = 5
			job.Labels["preset-service-account"] = "true"
		case RequirementRoot:
			job.Spec.Containers[0].SecurityContext.Privileged = newTrue()
		case RequirementKind:
			// Kind requires special volumes set up for docker
			dir := v1.HostPathDirectory
			job.Spec.Volumes = append(job.Spec.Volumes,
				v1.Volume{
					Name: "modules",
					VolumeSource: v1.VolumeSource{
						HostPath: &v1.HostPathVolumeSource{
							Path: "/lib/modules",
							Type: &dir,
						},
					},
				},
				v1.Volume{
					Name: "cgroup",
					VolumeSource: v1.VolumeSource{
						HostPath: &v1.HostPathVolumeSource{
							Path: "/sys/fs/cgroup",
							Type: &dir,
						},
					},
				},
			)
			job.Spec.Containers[0].VolumeMounts = append(job.Spec.Containers[0].VolumeMounts,
				v1.VolumeMount{
					MountPath: "/lib/modules",
					Name:      "modules",
					ReadOnly:  true,
				},
				v1.VolumeMount{
					MountPath: "/sys/fs/cgroup",
					Name:      "cgroup",
				},
			)
		}
	}
}

func applyModifiersPresubmit(presubmit *config.Presubmit, jobModifiers []string) {
	for _, modifier := range jobModifiers {
		if modifier == ModifierOptional {
			presubmit.Optional = true
		} else if modifier == ModifierHidden {
			presubmit.SkipReport = true
		} else if modifier == ModifierSkipped {
			presubmit.AlwaysRun = false
		}
	}
}

func applyModifiersPostsubmit(postsubmit *config.Postsubmit, jobModifiers []string) {
	for _, modifier := range jobModifiers {
		if modifier == ModifierOptional {
			// Does not exist on postsubmit
		} else if modifier == ModifierHidden {
			postsubmit.SkipReport = true
		}
		// Cannot skip a postsubmit; instead just make `type: presubmit`
	}
}

// Reads the generate job config for comparison
func ReadProwJobConfig(file string) config.JobConfig {
	yamlFile, err := ioutil.ReadFile(file)
	if err != nil {
		exit(err, "failed to read "+file)
	}
	jobs := config.JobConfig{}
	if err := yaml.Unmarshal(yamlFile, &jobs); err != nil {
		exit(err, "failed to unmarshal "+file)
	}
	return jobs
}

// kubernetes API requires a pointer to a bool for some reason
func newTrue() *bool {
	b := true
	return &b
}