package kubetest

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"text/template"

	"github.com/goccy/kubejob"
	"golang.org/x/sync/errgroup"
	"golang.org/x/xerrors"
	batchv1 "k8s.io/api/batch/v1"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	defaultDelimiter     = "\n"
	defaultConcurrentNum = 4
)

type DistributedTestJobBuilder struct {
	TestJobBuilder
	listCmd       []string
	listDelimiter string
	pattern       string
	podNum        int
}

func NewDistributedTestJobBuilder(clientset *kubernetes.Clientset, namespace string) *DistributedTestJobBuilder {
	return &DistributedTestJobBuilder{
		TestJobBuilder: TestJobBuilder{
			clientset: clientset,
			namespace: namespace,
		},
		listDelimiter: defaultDelimiter,
	}
}

func (b *DistributedTestJobBuilder) SetUser(user string) *DistributedTestJobBuilder {
	b.TestJobBuilder.SetUser(user)
	return b
}

func (b *DistributedTestJobBuilder) SetRepo(repo string) *DistributedTestJobBuilder {
	b.TestJobBuilder.SetRepo(repo)
	return b
}

func (b *DistributedTestJobBuilder) SetBranch(branch string) *DistributedTestJobBuilder {
	b.TestJobBuilder.SetBranch(branch)
	return b
}

func (b *DistributedTestJobBuilder) SetRev(rev string) *DistributedTestJobBuilder {
	b.TestJobBuilder.SetRev(rev)
	return b
}

func (b *DistributedTestJobBuilder) SetImage(image string) *DistributedTestJobBuilder {
	b.TestJobBuilder.SetImage(image)
	return b
}

func (b *DistributedTestJobBuilder) SetCommand(cmd []string) *DistributedTestJobBuilder {
	b.TestJobBuilder.SetCommand(cmd)
	return b
}

func (b *DistributedTestJobBuilder) SetToken(token string) *DistributedTestJobBuilder {
	b.TestJobBuilder.SetToken(token)
	return b
}

func (b *DistributedTestJobBuilder) SetTokenFromSecret(name string, key string) *DistributedTestJobBuilder {
	b.TestJobBuilder.SetTokenFromSecret(name, key)
	return b
}

func (b *DistributedTestJobBuilder) SetListCommand(list []string) *DistributedTestJobBuilder {
	b.listCmd = list
	return b
}

func (b *DistributedTestJobBuilder) SetListDelimiter(delim string) *DistributedTestJobBuilder {
	b.listDelimiter = delim
	return b
}

func (b *DistributedTestJobBuilder) SetTestNamePattern(pattern string) *DistributedTestJobBuilder {
	b.pattern = pattern
	return b
}

func (b *DistributedTestJobBuilder) SetPodNum(num int) *DistributedTestJobBuilder {
	b.podNum = num
	return b
}

func (b *DistributedTestJobBuilder) Build() (*DistributedTestJob, error) {
	testJob, err := b.TestJobBuilder.Build()
	if err != nil {
		return nil, xerrors.Errorf("failed to build testjob: %w", err)
	}
	listJob, err := NewTestJobBuilder(b.clientset, b.namespace).
		SetUser(b.user).
		SetRepo(b.repo).
		SetBranch(b.branch).
		SetRev(b.rev).
		SetImage(b.image).
		SetToken(testJob.token).
		SetCommand(b.listCmd).
		Build()
	listJob.DisablePrepareLog()
	listJob.DisableCommandLog()
	if err != nil {
		return nil, xerrors.Errorf("failed to build job for list command: %w", err)
	}
	var pattern *regexp.Regexp
	if b.pattern != "" {
		reg, err := regexp.Compile(b.pattern)
		if err != nil {
			return nil, xerrors.Errorf("failed to compile pattern %s: %w", b.pattern, err)
		}
		pattern = reg
	}
	return &DistributedTestJob{
		listJob:       listJob,
		testJob:       testJob,
		listDelimiter: b.listDelimiter,
		podNum:        b.podNum,
		pattern:       pattern,
	}, nil
}

type DistributedTestJob struct {
	listJob       *TestJob
	testJob       *TestJob
	listDelimiter string
	podNum        int
	pattern       *regexp.Regexp
}

func (t *DistributedTestJob) testCommand(cmdTmpl, test string) (string, error) {
	var cmd bytes.Buffer
	tmpl, err := template.New("").Parse(cmdTmpl)
	if err != nil {
		return "", xerrors.Errorf("failed to parse command template: %w", err)
	}
	if err := tmpl.Execute(&cmd, struct {
		Test string
	}{
		Test: test,
	}); err != nil {
		return "", xerrors.Errorf("failed to assign Test parameter to command template: %w", err)
	}
	return cmd.String(), nil
}

func (t *DistributedTestJob) testsToCommands(tests []string) ([]string, error) {
	cmdTmpl := strings.Join(t.testJob.cmd, " ")
	commands := []string{}
	for _, test := range tests {
		cmd, err := t.testCommand(cmdTmpl, test)
		if err != nil {
			return nil, xerrors.Errorf("failed to create test command: %w", err)
		}
		commands = append(commands, cmd)
	}
	return commands, nil
}

func (t *DistributedTestJob) Run(ctx context.Context) error {
	list, err := t.testList(ctx)
	if err != nil {
		return xerrors.Errorf("failed to get test list: %w", err)
	}
	plan := t.plan(list)
	var eg errgroup.Group
	for _, tests := range plan {
		commands, err := t.testsToCommands(tests)
		if err != nil {
			return xerrors.Errorf("failed to get commands from tests: %w", err)
		}
		eg.Go(func() error {
			if err := t.runTests(ctx, commands); err != nil {
				return xerrors.Errorf("failed to runTests: %w", err)
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return xerrors.Errorf("failed to distributed test job: %w", err)
	}
	return nil
}

func (t *DistributedTestJob) testCommandToContainer(test string) apiv1.Container {
	cmd := strings.Split(test, " ")
	volumeMount := t.testJob.sharedVolumeMount()
	return apiv1.Container{
		Image:        t.testJob.image,
		Command:      []string{cmd[0]},
		Args:         cmd[1:],
		WorkingDir:   volumeMount.MountPath,
		VolumeMounts: []apiv1.VolumeMount{volumeMount},
	}
}

func (t *DistributedTestJob) runTests(ctx context.Context, testCommands []string) error {
	concurrentNum := defaultConcurrentNum
	for i := 0; i < len(testCommands); i += concurrentNum {
		containers := []apiv1.Container{}
		for j := 0; j < concurrentNum; j++ {
			if i+j < len(testCommands) {
				containers = append(containers, t.testCommandToContainer(testCommands[i+j]))
			}
		}
		job, err := kubejob.NewJobBuilder(t.testJob.clientset, t.testJob.namespace).
			BuildWithJob(&batchv1.Job{
				Spec: batchv1.JobSpec{
					Template: apiv1.PodTemplateSpec{
						Spec: apiv1.PodSpec{
							Volumes:        []apiv1.Volume{t.testJob.sharedVolume()},
							InitContainers: t.testJob.initContainers(),
							Containers:     containers,
						},
					},
				},
			})
		if err != nil {
			return xerrors.Errorf("failed to build testjob: %w", err)
		}
		if err := job.Run(ctx); err != nil {
			return xerrors.Errorf("failed to run testjob: %w", err)
		}
	}
	return nil
}

func (t *DistributedTestJob) testList(ctx context.Context) ([]string, error) {
	job := t.listJob
	var b bytes.Buffer
	job.SetWriter(&b)
	if err := job.Run(ctx); err != nil {
		return nil, xerrors.Errorf("failed to get test list: %w", err)
	}
	result := b.String()
	if len(result) == 0 {
		return nil, xerrors.Errorf("could not find test list. list is empty")
	}
	delim := t.listDelimiter
	if delim == "" {
		delim = "\n"
	}
	tests := []string{}
	list := strings.Split(result, delim)
	if t.pattern != nil {
		for _, name := range list {
			if t.pattern.MatchString(name) {
				tests = append(tests, name)
			}
		}
	} else {
		tests = list
	}
	if len(tests) == 0 {
		return nil, xerrors.Errorf("could not find test list. list is invalid %s", result)
	}
	return tests, nil
}

func (t *DistributedTestJob) plan(list []string) [][]string {
	if len(list) < t.podNum {
		plan := make([][]string, len(list))
		for i := 0; i < len(list); i++ {
			plan[i] = []string{list[i]}
		}
		return plan
	}
	testNum := len(list) / t.podNum
	lastIdx := t.podNum - 1
	plan := [][]string{}
	sum := 0
	for i := 0; i < t.podNum; i++ {
		if i == lastIdx {
			plan = append(plan, list[sum:])
		} else {
			plan = append(plan, list[sum:sum+testNum])
		}
		sum += testNum
	}
	return plan
}
