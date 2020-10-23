// +build !ignore_autogenerated

package v1

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/goccy/kubejob"
	"github.com/rs/xid"
	"golang.org/x/sync/errgroup"
	"golang.org/x/xerrors"
	batchv1 "k8s.io/api/batch/v1"
	apiv1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
)

const (
	gitImageName         = "alpine/git"
	oauthTokenEnv        = "OAUTH_TOKEN"
	sharedVolumeName     = "repo"
	defaultListDelimiter = "\n"
)

var (
	ErrFailedTestJob = xerrors.New("failed test job")
)

type TestResult string

const (
	TestResultSuccess TestResult = "success"
	TestResultFailure TestResult = "failure"
)

type TestResultLog struct {
	TestResult     TestResult          `json:"testResult"`
	Job            string              `json:"job"`
	ElapsedTimeSec int                 `json:"elapsedTimeSec"`
	StartedAt      time.Time           `json:"startedAt"`
	Details        TestResultLogDetail `json:"details"`
}

type TestResultLogDetail struct {
	Tests []TestLog `json:"tests"`
}

type TestLog struct {
	Name           string     `json:"name"`
	TestResult     TestResult `json:"testResult"`
	ElapsedTimeSec int        `json:"elapsedTimeSec"`
	Message        string     `json:"-"`
}

type TestJobRunner struct {
	*kubernetes.Clientset
	token                     string
	disabledPrepareLog        bool
	disabledCommandLog        bool
	disabledResultLog         bool
	logger                    func(*kubejob.ContainerLog)
	containerNameToCommandMap sync.Map
}

func NewTestJobRunner(clientset *kubernetes.Clientset) *TestJobRunner {
	return &TestJobRunner{
		Clientset: clientset,
	}
}

func (r *TestJobRunner) SetToken(token string) {
	r.token = token
}

func (r *TestJobRunner) sharedVolume() apiv1.Volume {
	return apiv1.Volume{
		Name: sharedVolumeName,
		VolumeSource: apiv1.VolumeSource{
			EmptyDir: &apiv1.EmptyDirVolumeSource{},
		},
	}
}

func (r *TestJobRunner) sharedVolumeMount() apiv1.VolumeMount {
	return apiv1.VolumeMount{
		Name:      sharedVolumeName,
		MountPath: filepath.Join("/", "git", "workspace"),
	}
}

func (r *TestJobRunner) gitImage(job TestJob) string {
	if job.Spec.GitImage != "" {
		return job.Spec.GitImage
	}
	return gitImageName
}

func (r *TestJobRunner) cloneURL(job TestJob) string {
	repo := job.Spec.Repo
	if r.token != "" {
		return fmt.Sprintf("https://$(%s)@%s.git", oauthTokenEnv, repo)
	}
	return fmt.Sprintf("https://%s.git", repo)
}

func (r *TestJobRunner) gitCloneContainer(job TestJob) apiv1.Container {
	cloneURL := r.cloneURL(job)
	cloneCmd := []string{"clone"}
	volumeMount := r.sharedVolumeMount()
	branch := job.Spec.Branch
	if branch != "" {
		cloneCmd = append(cloneCmd, "-b", branch, cloneURL, volumeMount.MountPath)
	} else {
		cloneCmd = append(cloneCmd, cloneURL, volumeMount.MountPath)
	}
	return apiv1.Container{
		Name:         "kubetest-init-clone",
		Image:        r.gitImage(job),
		Command:      []string{"git"},
		Args:         cloneCmd,
		Env:          []apiv1.EnvVar{{Name: oauthTokenEnv, Value: r.token}},
		VolumeMounts: []apiv1.VolumeMount{volumeMount},
	}
}

func (r *TestJobRunner) gitSwitchContainer(job TestJob) apiv1.Container {
	volumeMount := r.sharedVolumeMount()
	return apiv1.Container{
		Name:         "kubetest-init-switch",
		Image:        r.gitImage(job),
		WorkingDir:   volumeMount.MountPath,
		Command:      []string{"git"},
		Args:         []string{"checkout", "--detach", job.Spec.Rev},
		VolumeMounts: []apiv1.VolumeMount{volumeMount},
	}
}

func (r *TestJobRunner) initContainers(job TestJob) []apiv1.Container {
	if job.Spec.Branch != "" {
		return []apiv1.Container{r.gitCloneContainer(job)}
	}
	return []apiv1.Container{
		r.gitCloneContainer(job),
		r.gitSwitchContainer(job),
	}
}

func (r *TestJobRunner) command(cmd Command) ([]string, []string) {
	e := base64.StdEncoding.EncodeToString([]byte(string(cmd)))
	return []string{"sh"}, []string{"-c", fmt.Sprintf("echo %s | base64 -d | sh", e)}
}

func (r *TestJobRunner) commandText(cmd Command) string {
	c, args := r.command(cmd)
	return strings.Join(append(c, args...), " ")
}

func (r *TestJobRunner) DisablePrepareLog() {
	r.disabledPrepareLog = true
}

func (r *TestJobRunner) DisableCommandLog() {
	r.disabledCommandLog = true
}

func (r *TestJobRunner) DisableResultLog() {
	r.disabledResultLog = true
}

func (r *TestJobRunner) SetLogger(logger func(*kubejob.ContainerLog)) {
	r.logger = logger
}

func (r *TestJobRunner) Run(ctx context.Context, testjob TestJob) error {
	testLog := TestResultLog{Job: testjob.ObjectMeta.Name, StartedAt: time.Now()}

	defer func(start time.Time) {
		if r.disabledResultLog {
			return
		}
		testLog.ElapsedTimeSec = int(time.Since(start).Seconds())
		b, _ := json.Marshal(testLog)

		var logMap map[string]interface{}
		json.Unmarshal(b, &logMap)

		for k, v := range testjob.Spec.Log {
			logMap[k] = v
		}
		b, _ = json.Marshal(logMap)
		fmt.Println(string(b))
	}(time.Now())

	testLogs, err := r.run(ctx, testjob)
	testLog.Details = TestResultLogDetail{
		Tests: testLogs,
	}
	if err != nil {
		testLog.TestResult = TestResultFailure
		return err
	}
	testLog.TestResult = TestResultSuccess
	return nil
}

func (r *TestJobRunner) run(ctx context.Context, testjob TestJob) ([]TestLog, error) {
	if testjob.Spec.Branch == "" && testjob.Spec.Rev == "" {
		testjob.Spec.Branch = "master"
	}
	token := testjob.Spec.Token
	if token != nil {
		secret, err := r.CoreV1().
			Secrets(testjob.Namespace).
			Get(token.SecretKeyRef.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		data, exists := secret.Data[token.SecretKeyRef.Key]
		if !exists {
			gr := schema.GroupResource{
				Group:    GroupVersion.Group,
				Resource: "TestJob",
			}
			return nil, errors.NewNotFound(gr, "token")
		}
		r.token = strings.TrimSpace(string(data))
	}
	if err := r.prepare(ctx, testjob); err != nil {
		return nil, err
	}
	if testjob.Spec.DistributedTest != nil {
		return r.runDistributedTest(ctx, testjob)
	}
	return r.runTest(ctx, testjob)
}

func (r *TestJobRunner) prepareImage(stepIdx int, testjob TestJob) string {
	step := testjob.Spec.Prepare.Steps[stepIdx]
	if step.Image != "" {
		return step.Image
	}
	image := testjob.Spec.Prepare.Image
	if image != "" {
		return image
	}
	return testjob.Spec.Image
}

func (r *TestJobRunner) prepareWorkingDir(stepIdx int, testjob TestJob) string {
	step := testjob.Spec.Prepare.Steps[stepIdx]
	if step.Workdir != "" {
		return step.Workdir
	}
	return r.sharedVolumeMount().MountPath
}

func (r *TestJobRunner) prepareEnv(stepIdx int, testjob TestJob) []apiv1.EnvVar {
	step := testjob.Spec.Prepare.Steps[stepIdx]
	env := step.Env
	env = append(env, testjob.Spec.Env...)
	return env
}

func (r *TestJobRunner) enabledPrepareCheckout(testjob TestJob) bool {
	checkout := testjob.Spec.Prepare.Checkout
	if checkout != nil && !(*checkout) {
		return false
	}
	return true
}

func (r *TestJobRunner) enabledCheckout(testjob TestJob) bool {
	checkout := testjob.Spec.Checkout
	if checkout != nil && !(*checkout) {
		return false
	}
	return true
}

func (r *TestJobRunner) generateName(name string) string {
	return fmt.Sprintf("%s-%s", name, xid.New())
}

func (r *TestJobRunner) prepare(ctx context.Context, testjob TestJob) error {
	if len(testjob.Spec.Prepare.Steps) == 0 {
		return nil
	}
	var containers []apiv1.Container
	if r.enabledPrepareCheckout(testjob) {
		containers = r.initContainers(testjob)
	}
	fmt.Println("run prepare")
	startPrepareTime := time.Now()
	defer func() {
		fmt.Fprintf(os.Stderr, "prepare: elapsed time %f sec\n", time.Since(startPrepareTime).Seconds())
	}()
	for stepIdx, step := range testjob.Spec.Prepare.Steps {
		image := r.prepareImage(stepIdx, testjob)
		cmd, args := r.command(step.Command)
		volumeMount := r.sharedVolumeMount()
		containers = append(containers, apiv1.Container{
			Name:       step.Name,
			Image:      image,
			Command:    cmd,
			Args:       args,
			WorkingDir: r.prepareWorkingDir(stepIdx, testjob),
			VolumeMounts: []apiv1.VolumeMount{
				volumeMount,
			},
			Env: r.prepareEnv(stepIdx, testjob),
		})
	}
	lastContainer := containers[len(containers)-1]
	initContainers := []apiv1.Container{}
	if len(containers) > 1 {
		initContainers = containers[:len(containers)-1]
	}
	job, err := kubejob.NewJobBuilder(r.Clientset, testjob.Namespace).
		BuildWithJob(&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name: r.generateName(testjob.ObjectMeta.Name),
			},
			Spec: batchv1.JobSpec{
				Template: apiv1.PodTemplateSpec{
					Spec: apiv1.PodSpec{
						Volumes: []apiv1.Volume{
							r.sharedVolume(),
						},
						InitContainers:   initContainers,
						Containers:       []apiv1.Container{lastContainer},
						ImagePullSecrets: testjob.Spec.ImagePullSecrets,
					},
				},
			},
		})
	if err != nil {
		return err
	}
	job.DisableCommandLog()
	if r.logger != nil {
		job.SetLogger(r.logger)
	}
	return job.Run(ctx)
}

func (r *TestJobRunner) newJobForTesting(testjob TestJob, containers []apiv1.Container) (*kubejob.Job, error) {
	var initContainers []apiv1.Container
	if r.enabledCheckout(testjob) {
		initContainers = r.initContainers(testjob)
	}
	volumes := testjob.Spec.Volumes
	volumes = append(volumes, r.sharedVolume())
	return kubejob.NewJobBuilder(r.Clientset, testjob.Namespace).
		BuildWithJob(&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name: r.generateName(testjob.ObjectMeta.Name),
			},
			Spec: batchv1.JobSpec{
				Template: apiv1.PodTemplateSpec{
					Spec: apiv1.PodSpec{
						Volumes:          volumes,
						InitContainers:   initContainers,
						Containers:       containers,
						ImagePullSecrets: testjob.Spec.ImagePullSecrets,
					},
				},
			},
		})
}

func (r *TestJobRunner) runTest(ctx context.Context, testjob TestJob) ([]TestLog, error) {
	job, err := r.newJobForTesting(testjob, []apiv1.Container{r.testjobToContainer(testjob)})
	if err != nil {
		return nil, err
	}
	if r.logger != nil {
		job.SetLogger(r.logger)
	}
	if r.disabledPrepareLog {
		job.DisableInitContainerLog()
	}
	if r.disabledCommandLog {
		job.DisableCommandLog()
	}
	if err := job.Run(ctx); err != nil {
		var failedJob *kubejob.FailedJob
		if xerrors.As(err, &failedJob) {
			return nil, ErrFailedTestJob
		}
		log.Printf(err.Error())
		return nil, ErrFailedTestJob
	}
	return nil, nil
}

func (r *TestJobRunner) runDistributedTest(ctx context.Context, testjob TestJob) ([]TestLog, error) {
	fmt.Println("get listing of tests...")
	list, err := r.testList(ctx, testjob)
	if err != nil {
		return nil, xerrors.Errorf("failed to get list for testing: %w", err)
	}
	if len(list) == 0 {
		return nil, nil
	}
	plan := r.plan(testjob, list)

	defer func(start time.Time) {
		fmt.Fprintf(os.Stderr, "test: elapsed time %f sec\n", time.Since(start).Seconds())
	}(time.Now())

	failedTestCommands := []*command{}

	var (
		loggerMu      sync.Mutex
		failedTestsMu sync.Mutex
		lastPodIdx    int
	)
	containerNameToLogMap := map[string][]string{}
	podNameToIndexMap := map[string]int{}
	testLogMap := map[string]TestLog{}
	logger := func(log *kubejob.ContainerLog) {
		loggerMu.Lock()
		defer loggerMu.Unlock()

		name := log.Container.Name
		if log.IsFinished {
			cmd, _ := r.containerNameToCommandMap.Load(name)
			logs, exists := containerNameToLogMap[name]
			if exists {
				podName := log.Pod.Name
				idx, exists := podNameToIndexMap[podName]
				if !exists {
					idx = lastPodIdx
					podNameToIndexMap[log.Pod.Name] = lastPodIdx
					lastPodIdx++
				}
				if cmd != nil {
					c := cmd.(*command)
					fmt.Fprintf(os.Stderr, "[POD %d] TEST=%s %s", idx, c.test, testjob.Spec.Command)
					testLogMap[c.test] = TestLog{
						Name:           c.test,
						TestResult:     TestResultSuccess,
						ElapsedTimeSec: int(time.Since(c.startedAt).Seconds()),
						Message:        strings.Join(logs, "\n"),
					}
				}
				for _, log := range logs {
					fmt.Fprintf(os.Stderr, "[POD %d] %s", idx, log)
				}
				fmt.Fprintf(os.Stderr, "\n")
			}
			delete(containerNameToLogMap, name)
		} else {
			value, exists := containerNameToLogMap[name]
			logs := []string{}
			if exists {
				logs = value
			} else {
				cmd, _ := r.containerNameToCommandMap.Load(name)
				if cmd != nil {
					startedAt := time.Now()
					for _, status := range log.Pod.Status.ContainerStatuses {
						if log.Container.Name != status.Name {
							continue
						}
						running := status.State.Running
						if running == nil {
							continue
						}
						startedAt = running.StartedAt.Time
					}
					cmd.(*command).startedAt = startedAt
				}
			}
			logs = append(logs, log.Log)
			containerNameToLogMap[name] = logs
		}
	}

	var eg errgroup.Group
	for _, tests := range plan {
		tests := tests
		commands := r.testsToCommands(testjob, tests)
		eg.Go(func() error {
			commands, err := r.runTests(ctx, testjob, logger, commands)
			if err != nil {
				return xerrors.Errorf("failed to runTests: %w", err)
			}
			if len(commands) > 0 {
				failedTestsMu.Lock()
				failedTestCommands = append(failedTestCommands, commands...)
				failedTestsMu.Unlock()
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return nil, xerrors.Errorf("failed to distributed test job: %w", err)
	}

	if len(failedTestCommands) > 0 {
		for _, command := range failedTestCommands {
			log := testLogMap[command.test]
			log.TestResult = TestResultFailure
			testLogMap[command.test] = log
		}
		logs := []TestLog{}
		for _, log := range testLogMap {
			logs = append(logs, log)
		}
		if !testjob.Spec.DistributedTest.Retest {
			return logs, ErrFailedTestJob
		}
		fmt.Println("start retest....")
		tests := []string{}
		for _, command := range failedTestCommands {
			tests = append(tests, command.test)
		}
		concatedTests := strings.Join(tests, testjob.Spec.DistributedTest.RetestDelimiter)
		command, args := r.command(testjob.Spec.Command)
		cmd := r.testCommand(command, args, concatedTests)
		failedTests, err := r.runTests(ctx, testjob, logger, commands{cmd})
		if err != nil {
			return logs, xerrors.Errorf("failed test: %w", err)
		}
		if len(failedTests) > 0 {
			return logs, ErrFailedTestJob
		}
	}
	logs := []TestLog{}
	for _, log := range testLogMap {
		logs = append(logs, log)
	}
	return logs, nil
}

type command struct {
	cmd       []string
	args      []string
	test      string
	container string
	startedAt time.Time
}

type commands []*command

func (c commands) commandValueMap() map[string]*command {
	m := map[string]*command{}
	for _, cc := range c {
		m[cc.test] = cc
	}
	return m
}

func (r *TestJobRunner) testCommand(cmd []string, args []string, test string) *command {
	return &command{
		cmd:  cmd,
		args: args,
		test: test,
	}
}

func (r *TestJobRunner) testsToCommands(job TestJob, tests []string) []*command {
	c, args := r.command(job.Spec.Command)
	commands := []*command{}
	for _, test := range tests {
		cmd := r.testCommand(c, args, test)
		commands = append(commands, cmd)
	}
	return commands
}

func (r *TestJobRunner) workingDir(testjob TestJob) string {
	if testjob.Spec.Workdir != "" {
		return testjob.Spec.Workdir
	}
	return r.sharedVolumeMount().MountPath
}

func (r *TestJobRunner) testjobToContainer(testjob TestJob) apiv1.Container {
	cmd, args := r.command(testjob.Spec.Command)
	volumeMount := r.sharedVolumeMount()
	return apiv1.Container{
		Image:      testjob.Spec.Image,
		Command:    cmd,
		Args:       args,
		WorkingDir: r.workingDir(testjob),
		VolumeMounts: []apiv1.VolumeMount{
			volumeMount,
		},
		Env: testjob.Spec.Env,
	}
}

func (r *TestJobRunner) testCommandToContainer(job TestJob, test *command) apiv1.Container {
	env := []apiv1.EnvVar{}
	env = append(env, job.Spec.Env...)
	env = append(env, apiv1.EnvVar{
		Name:  "TEST",
		Value: test.test,
	})
	return apiv1.Container{
		Image:        job.Spec.Image,
		Command:      test.cmd,
		Args:         test.args,
		WorkingDir:   r.workingDir(job),
		VolumeMounts: append(job.Spec.VolumeMounts, r.sharedVolumeMount()),
		Env:          env,
	}
}

func (r *TestJobRunner) runTests(ctx context.Context, testjob TestJob, logger kubejob.Logger, testCommands commands) ([]*command, error) {
	commandValueMap := testCommands.commandValueMap()
	containers := []apiv1.Container{}
	for i := 0; i < len(testCommands); i++ {
		containers = append(containers, r.testCommandToContainer(testjob, testCommands[i]))
	}
	job, err := r.newJobForTesting(testjob, containers)
	if err != nil {
		return nil, err
	}
	for _, cache := range testjob.Spec.DistributedTest.Cache {
		cmd, args := r.command(cache.Command)
		volumeMounts := append(testjob.Spec.VolumeMounts, r.sharedVolumeMount(), apiv1.VolumeMount{
			Name:      cache.Name,
			MountPath: cache.Path,
		})
		cacheContainer := apiv1.Container{
			Name:         cache.Name,
			Image:        testjob.Spec.Image,
			Command:      cmd,
			Args:         args,
			WorkingDir:   r.workingDir(testjob),
			VolumeMounts: volumeMounts,
			Env:          testjob.Spec.Env,
		}
		job.Spec.Template.Spec.Volumes = append(job.Spec.Template.Spec.Volumes, apiv1.Volume{
			Name: cache.Name,
			VolumeSource: apiv1.VolumeSource{
				EmptyDir: &apiv1.EmptyDirVolumeSource{},
			},
		})
		job.Spec.Template.Spec.InitContainers = append(job.Spec.Template.Spec.InitContainers, cacheContainer)
	}
	for i := 0; i < len(testCommands); i++ {
		containerName := job.Spec.Template.Spec.Containers[i].Name
		testCommands[i].container = containerName
		r.containerNameToCommandMap.Store(containerName, testCommands[i])
	}
	job.DisableCommandLog()
	job.SetLogger(logger)
	failedTestCommands := []*command{}
	if err := job.Run(ctx); err != nil {
		var failedJob *kubejob.FailedJob
		if xerrors.As(err, &failedJob) {
			for _, container := range failedJob.FailedContainers() {
				var testName string
				for _, env := range container.Env {
					if env.Name == "TEST" {
						testName = env.Value
						break
					}
				}
				command := commandValueMap[testName]
				failedTestCommands = append(failedTestCommands, command)
			}
		} else {
			return nil, err
		}
	}
	return failedTestCommands, nil
}

func (r *TestJobRunner) testList(ctx context.Context, testjob TestJob) ([]string, error) {
	startListTime := time.Now()
	defer func() {
		fmt.Fprintf(os.Stderr, "list: elapsed time %f sec\n", time.Since(startListTime).Seconds())
	}()
	distributedTest := testjob.Spec.DistributedTest

	listjob := testjob
	listjob.Spec.Command = distributedTest.ListCommand
	listjob.Spec.Prepare.Steps = []PrepareStepSpec{}
	listjob.Spec.DistributedTest = nil

	listJobRunner := NewTestJobRunner(r.Clientset)
	listJobRunner.DisablePrepareLog()
	listJobRunner.DisableCommandLog()
	listJobRunner.DisableResultLog()

	var pattern *regexp.Regexp
	if distributedTest.Pattern != "" {
		reg, err := regexp.Compile(distributedTest.Pattern)
		if err != nil {
			return nil, xerrors.Errorf("failed to compile pattern for distributed testing: %w", err)
		}
		pattern = reg
	}

	var b bytes.Buffer
	listJobRunner.SetLogger(func(log *kubejob.ContainerLog) {
		b.WriteString(log.Log)
	})
	if err := listJobRunner.Run(ctx, listjob); err != nil {
		return nil, xerrors.Errorf("failed to run listJob %s: %w", b.String(), err)
	}
	delim := distributedTest.ListDelimiter
	if delim == "" {
		delim = "\n"
	}
	tests := []string{}
	result := b.String()
	list := strings.Split(result, delim)
	if pattern != nil {
		for _, name := range list {
			if pattern.MatchString(name) {
				tests = append(tests, name)
			}
		}
	} else {
		tests = list
	}
	return tests, nil
}

func (r *TestJobRunner) plan(job TestJob, list []string) [][]string {
	maxContainers := job.Spec.DistributedTest.MaxContainersPerPod

	if len(list) <= maxContainers {
		return [][]string{list}
	}
	concurrent := len(list) / maxContainers
	plan := [][]string{}
	sum := 0
	for i := 0; i <= concurrent; i++ {
		if i == concurrent {
			plan = append(plan, list[sum:])
		} else {
			plan = append(plan, list[sum:sum+maxContainers])
		}
		sum += maxContainers
	}
	return plan
}
