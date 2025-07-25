/*
   Copyright 2020 Docker Compose CLI authors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package compose

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/containerd/platforms"
	containerType "github.com/docker/docker/api/types/container"
	mmount "github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/versions"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/sirupsen/logrus"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	"github.com/docker/compose/v2/internal/tracing"
	"github.com/docker/compose/v2/pkg/api"
	"github.com/docker/compose/v2/pkg/progress"
	"github.com/docker/compose/v2/pkg/utils"
)

const (
	doubledContainerNameWarning = "WARNING: The %q service is using the custom container name %q. " +
		"Docker requires each container to have a unique name. " +
		"Remove the custom name to scale the service"
)

// convergence manages service's container lifecycle.
// Based on initially observed state, it reconciles the existing container with desired state, which might include
// re-creating container, adding or removing replicas, or starting stopped containers.
// Cross services dependencies are managed by creating services in expected order and updating `service:xx` reference
// when a service has converged, so dependent ones can be managed with resolved containers references.
type convergence struct {
	service    *composeService
	services   map[string]Containers
	networks   map[string]string
	volumes    map[string]string
	stateMutex sync.Mutex
}

func (c *convergence) getObservedState(serviceName string) Containers {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()
	return c.services[serviceName]
}

func (c *convergence) setObservedState(serviceName string, containers Containers) {
	c.stateMutex.Lock()
	defer c.stateMutex.Unlock()
	c.services[serviceName] = containers
}

func newConvergence(services []string, state Containers, networks map[string]string, volumes map[string]string, s *composeService) *convergence {
	observedState := map[string]Containers{}
	for _, s := range services {
		observedState[s] = Containers{}
	}
	for _, c := range state.filter(isNotOneOff) {
		service := c.Labels[api.ServiceLabel]
		observedState[service] = append(observedState[service], c)
	}
	return &convergence{
		service:  s,
		services: observedState,
		networks: networks,
		volumes:  volumes,
	}
}

func (c *convergence) apply(ctx context.Context, project *types.Project, options api.CreateOptions) error {
	return InDependencyOrder(ctx, project, func(ctx context.Context, name string) error {
		service, err := project.GetService(name)
		if err != nil {
			return err
		}

		return tracing.SpanWrapFunc("service/apply", tracing.ServiceOptions(service), func(ctx context.Context) error {
			strategy := options.RecreateDependencies
			if slices.Contains(options.Services, name) {
				strategy = options.Recreate
			}
			return c.ensureService(ctx, project, service, strategy, options.Inherit, options.Timeout)
		})(ctx)
	})
}

func (c *convergence) ensureService(ctx context.Context, project *types.Project, service types.ServiceConfig, recreate string, inherit bool, timeout *time.Duration) error { //nolint:gocyclo
	if service.Provider != nil {
		return c.service.runPlugin(ctx, project, service, "up")
	}
	expected, err := getScale(service)
	if err != nil {
		return err
	}
	containers := c.getObservedState(service.Name)
	actual := len(containers)
	updated := make(Containers, expected)

	eg, _ := errgroup.WithContext(ctx)

	err = c.resolveServiceReferences(&service)
	if err != nil {
		return err
	}

	sort.Slice(containers, func(i, j int) bool {
		// select obsolete containers first, so they get removed as we scale down
		if obsolete, _ := c.mustRecreate(service, containers[i], recreate); obsolete {
			// i is obsolete, so must be first in the list
			return true
		}
		if obsolete, _ := c.mustRecreate(service, containers[j], recreate); obsolete {
			// j is obsolete, so must be first in the list
			return false
		}

		// For up-to-date containers, sort by container number to preserve low-values in container numbers
		ni, erri := strconv.Atoi(containers[i].Labels[api.ContainerNumberLabel])
		nj, errj := strconv.Atoi(containers[j].Labels[api.ContainerNumberLabel])
		if erri == nil && errj == nil {
			return ni > nj
		}

		// If we don't get a container number (?) just sort by creation date
		return containers[i].Created < containers[j].Created
	})

	slices.Reverse(containers)
	for i, container := range containers {
		if i >= expected {
			// Scale Down
			// As we sorted containers, obsolete ones and/or highest number will be removed
			container := container
			traceOpts := append(tracing.ServiceOptions(service), tracing.ContainerOptions(container)...)
			eg.Go(tracing.SpanWrapFuncForErrGroup(ctx, "service/scale/down", traceOpts, func(ctx context.Context) error {
				return c.service.stopAndRemoveContainer(ctx, container, &service, timeout, false)
			}))
			continue
		}

		mustRecreate, err := c.mustRecreate(service, container, recreate)
		if err != nil {
			return err
		}
		if mustRecreate {
			err := c.stopDependentContainers(ctx, project, service)
			if err != nil {
				return err
			}

			i, container := i, container
			eg.Go(tracing.SpanWrapFuncForErrGroup(ctx, "container/recreate", tracing.ContainerOptions(container), func(ctx context.Context) error {
				recreated, err := c.service.recreateContainer(ctx, project, service, container, inherit, timeout)
				updated[i] = recreated
				return err
			}))
			continue
		}

		// Enforce non-diverged containers are running
		w := progress.ContextWriter(ctx)
		name := getContainerProgressName(container)
		switch container.State {
		case ContainerRunning:
			w.Event(progress.RunningEvent(name))
		case ContainerCreated:
		case ContainerRestarting:
		case ContainerExited:
		default:
			container := container
			eg.Go(tracing.EventWrapFuncForErrGroup(ctx, "service/start", tracing.ContainerOptions(container), func(ctx context.Context) error {
				return c.service.startContainer(ctx, container)
			}))
		}
		updated[i] = container
	}

	next := nextContainerNumber(containers)
	for i := 0; i < expected-actual; i++ {
		// Scale UP
		number := next + i
		name := getContainerName(project.Name, service, number)
		eventOpts := tracing.SpanOptions{trace.WithAttributes(attribute.String("container.name", name))}
		eg.Go(tracing.EventWrapFuncForErrGroup(ctx, "service/scale/up", eventOpts, func(ctx context.Context) error {
			opts := createOptions{
				AutoRemove:        false,
				AttachStdin:       false,
				UseNetworkAliases: true,
				Labels:            mergeLabels(service.Labels, service.CustomLabels),
			}
			container, err := c.service.createContainer(ctx, project, service, name, number, opts)
			updated[actual+i] = container
			return err
		}))
		continue
	}

	err = eg.Wait()
	c.setObservedState(service.Name, updated)
	return err
}

func (c *convergence) stopDependentContainers(ctx context.Context, project *types.Project, service types.ServiceConfig) error {
	// Stop dependent containers, so they will be restarted after service is re-created
	dependents := project.GetDependentsForService(service, func(dependency types.ServiceDependency) bool {
		return dependency.Restart
	})
	if len(dependents) == 0 {
		return nil
	}
	err := c.service.stop(ctx, project.Name, api.StopOptions{
		Services: dependents,
		Project:  project,
	}, nil)
	if err != nil {
		return err
	}

	for _, name := range dependents {
		dependentStates := c.getObservedState(name)
		for i, dependent := range dependentStates {
			dependent.State = ContainerExited
			dependentStates[i] = dependent
		}
		c.setObservedState(name, dependentStates)
	}
	return nil
}

func getScale(config types.ServiceConfig) (int, error) {
	scale := config.GetScale()
	if scale > 1 && config.ContainerName != "" {
		return 0, fmt.Errorf(doubledContainerNameWarning,
			config.Name,
			config.ContainerName)
	}
	return scale, nil
}

// resolveServiceReferences replaces reference to another service with reference to an actual container
func (c *convergence) resolveServiceReferences(service *types.ServiceConfig) error {
	err := c.resolveVolumeFrom(service)
	if err != nil {
		return err
	}

	err = c.resolveSharedNamespaces(service)
	if err != nil {
		return err
	}
	return nil
}

func (c *convergence) resolveVolumeFrom(service *types.ServiceConfig) error {
	for i, vol := range service.VolumesFrom {
		spec := strings.Split(vol, ":")
		if len(spec) == 0 {
			continue
		}
		if spec[0] == "container" {
			service.VolumesFrom[i] = spec[1]
			continue
		}
		name := spec[0]
		dependencies := c.getObservedState(name)
		if len(dependencies) == 0 {
			return fmt.Errorf("cannot share volume with service %s: container missing", name)
		}
		service.VolumesFrom[i] = dependencies.sorted()[0].ID
	}
	return nil
}

func (c *convergence) resolveSharedNamespaces(service *types.ServiceConfig) error {
	str := service.NetworkMode
	if name := getDependentServiceFromMode(str); name != "" {
		dependencies := c.getObservedState(name)
		if len(dependencies) == 0 {
			return fmt.Errorf("cannot share network namespace with service %s: container missing", name)
		}
		service.NetworkMode = types.ContainerPrefix + dependencies.sorted()[0].ID
	}

	str = service.Ipc
	if name := getDependentServiceFromMode(str); name != "" {
		dependencies := c.getObservedState(name)
		if len(dependencies) == 0 {
			return fmt.Errorf("cannot share IPC namespace with service %s: container missing", name)
		}
		service.Ipc = types.ContainerPrefix + dependencies.sorted()[0].ID
	}

	str = service.Pid
	if name := getDependentServiceFromMode(str); name != "" {
		dependencies := c.getObservedState(name)
		if len(dependencies) == 0 {
			return fmt.Errorf("cannot share PID namespace with service %s: container missing", name)
		}
		service.Pid = types.ContainerPrefix + dependencies.sorted()[0].ID
	}

	return nil
}

func (c *convergence) mustRecreate(expected types.ServiceConfig, actual containerType.Summary, policy string) (bool, error) {
	if policy == api.RecreateNever {
		return false, nil
	}
	if policy == api.RecreateForce {
		return true, nil
	}
	configHash, err := ServiceHash(expected)
	if err != nil {
		return false, err
	}
	configChanged := actual.Labels[api.ConfigHashLabel] != configHash
	imageUpdated := actual.Labels[api.ImageDigestLabel] != expected.CustomLabels[api.ImageDigestLabel]
	if configChanged || imageUpdated {
		return true, nil
	}

	if c.networks != nil && actual.State == "running" {
		if checkExpectedNetworks(expected, actual, c.networks) {
			return true, nil
		}
	}

	if c.volumes != nil {
		if checkExpectedVolumes(expected, actual, c.volumes) {
			return true, nil
		}
	}

	return false, nil
}

func checkExpectedNetworks(expected types.ServiceConfig, actual containerType.Summary, networks map[string]string) bool {
	// check the networks container is connected to are the expected ones
	for net := range expected.Networks {
		id := networks[net]
		if id == "swarm" {
			// corner-case : swarm overlay network isn't visible until a container is attached
			continue
		}
		found := false
		for _, settings := range actual.NetworkSettings.Networks {
			if settings.NetworkID == id {
				found = true
				break
			}
		}
		if !found {
			// config is up-to-date but container is not connected to network
			return true
		}
	}
	return false
}

func checkExpectedVolumes(expected types.ServiceConfig, actual containerType.Summary, volumes map[string]string) bool {
	// check container's volume mounts and search for the expected ones
	for _, vol := range expected.Volumes {
		if vol.Type != string(mmount.TypeVolume) {
			continue
		}
		if vol.Source == "" {
			continue
		}
		id := volumes[vol.Source]
		found := false
		for _, mount := range actual.Mounts {
			if mount.Type != mmount.TypeVolume {
				continue
			}
			if mount.Name == id {
				found = true
				break
			}
		}
		if !found {
			// config is up-to-date but container doesn't have volume mounted
			return true
		}
	}
	return false
}

func getContainerName(projectName string, service types.ServiceConfig, number int) string {
	name := getDefaultContainerName(projectName, service.Name, strconv.Itoa(number))
	if service.ContainerName != "" {
		name = service.ContainerName
	}
	return name
}

func getDefaultContainerName(projectName, serviceName, index string) string {
	return strings.Join([]string{projectName, serviceName, index}, api.Separator)
}

func getContainerProgressName(ctr containerType.Summary) string {
	return "Container " + getCanonicalContainerName(ctr)
}

func containerEvents(containers Containers, eventFunc func(string) progress.Event) []progress.Event {
	events := []progress.Event{}
	for _, ctr := range containers {
		events = append(events, eventFunc(getContainerProgressName(ctr)))
	}
	return events
}

func containerReasonEvents(containers Containers, eventFunc func(string, string) progress.Event, reason string) []progress.Event {
	events := []progress.Event{}
	for _, ctr := range containers {
		events = append(events, eventFunc(getContainerProgressName(ctr), reason))
	}
	return events
}

// ServiceConditionRunningOrHealthy is a service condition on status running or healthy
const ServiceConditionRunningOrHealthy = "running_or_healthy"

//nolint:gocyclo
func (s *composeService) waitDependencies(ctx context.Context, project *types.Project, dependant string, dependencies types.DependsOnConfig, containers Containers, timeout time.Duration) error {
	if timeout > 0 {
		withTimeout, cancelFunc := context.WithTimeout(ctx, timeout)
		defer cancelFunc()
		ctx = withTimeout
	}
	eg, _ := errgroup.WithContext(ctx)
	w := progress.ContextWriter(ctx)
	for dep, config := range dependencies {
		if shouldWait, err := shouldWaitForDependency(dep, config, project); err != nil {
			return err
		} else if !shouldWait {
			continue
		}

		waitingFor := containers.filter(isService(dep), isNotOneOff)
		w.Events(containerEvents(waitingFor, progress.Waiting))
		if len(waitingFor) == 0 {
			if config.Required {
				return fmt.Errorf("%s is missing dependency %s", dependant, dep)
			}
			logrus.Warnf("%s is missing dependency %s", dependant, dep)
			continue
		}

		eg.Go(func() error {
			ticker := time.NewTicker(500 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
				case <-ctx.Done():
					return nil
				}
				switch config.Condition {
				case ServiceConditionRunningOrHealthy:
					healthy, err := s.isServiceHealthy(ctx, waitingFor, true)
					if err != nil {
						if !config.Required {
							w.Events(containerReasonEvents(waitingFor, progress.SkippedEvent, fmt.Sprintf("optional dependency %q is not running or is unhealthy", dep)))
							logrus.Warnf("optional dependency %q is not running or is unhealthy: %s", dep, err.Error())
							return nil
						}
						return err
					}
					if healthy {
						w.Events(containerEvents(waitingFor, progress.Healthy))
						return nil
					}
				case types.ServiceConditionHealthy:
					healthy, err := s.isServiceHealthy(ctx, waitingFor, false)
					if err != nil {
						if !config.Required {
							w.Events(containerReasonEvents(waitingFor, progress.SkippedEvent, fmt.Sprintf("optional dependency %q failed to start", dep)))
							logrus.Warnf("optional dependency %q failed to start: %s", dep, err.Error())
							return nil
						}
						w.Events(containerEvents(waitingFor, progress.ErrorEvent))
						return fmt.Errorf("dependency failed to start: %w", err)
					}
					if healthy {
						w.Events(containerEvents(waitingFor, progress.Healthy))
						return nil
					}
				case types.ServiceConditionCompletedSuccessfully:
					exited, code, err := s.isServiceCompleted(ctx, waitingFor)
					if err != nil {
						return err
					}
					if exited {
						if code == 0 {
							w.Events(containerEvents(waitingFor, progress.Exited))
							return nil
						}

						messageSuffix := fmt.Sprintf("%q didn't complete successfully: exit %d", dep, code)
						if !config.Required {
							// optional -> mark as skipped & don't propagate error
							w.Events(containerReasonEvents(waitingFor, progress.SkippedEvent, fmt.Sprintf("optional dependency %s", messageSuffix)))
							logrus.Warnf("optional dependency %s", messageSuffix)
							return nil
						}

						msg := fmt.Sprintf("service %s", messageSuffix)
						w.Events(containerReasonEvents(waitingFor, progress.ErrorMessageEvent, msg))
						return errors.New(msg)
					}
				default:
					logrus.Warnf("unsupported depends_on condition: %s", config.Condition)
					return nil
				}
			}
		})
	}
	err := eg.Wait()
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("timeout waiting for dependencies")
	}
	return err
}

func shouldWaitForDependency(serviceName string, dependencyConfig types.ServiceDependency, project *types.Project) (bool, error) {
	if dependencyConfig.Condition == types.ServiceConditionStarted {
		// already managed by InDependencyOrder
		return false, nil
	}
	if service, err := project.GetService(serviceName); err != nil {
		for _, ds := range project.DisabledServices {
			if ds.Name == serviceName {
				// don't wait for disabled service (--no-deps)
				return false, nil
			}
		}
		return false, err
	} else if service.GetScale() == 0 {
		// don't wait for the dependency which configured to have 0 containers running
		return false, nil
	} else if service.Provider != nil {
		// don't wait for provider services
		return false, nil
	}
	return true, nil
}

func nextContainerNumber(containers []containerType.Summary) int {
	maxNumber := 0
	for _, c := range containers {
		s, ok := c.Labels[api.ContainerNumberLabel]
		if !ok {
			logrus.Warnf("container %s is missing %s label", c.ID, api.ContainerNumberLabel)
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			logrus.Warnf("container %s has invalid %s label: %s", c.ID, api.ContainerNumberLabel, s)
			continue
		}
		if n > maxNumber {
			maxNumber = n
		}
	}
	return maxNumber + 1
}

func (s *composeService) createContainer(ctx context.Context, project *types.Project, service types.ServiceConfig,
	name string, number int, opts createOptions,
) (ctr containerType.Summary, err error) {
	w := progress.ContextWriter(ctx)
	eventName := "Container " + name
	w.Event(progress.CreatingEvent(eventName))
	ctr, err = s.createMobyContainer(ctx, project, service, name, number, nil, opts, w)
	if err != nil {
		if ctx.Err() == nil {
			w.Event(progress.Event{
				ID:         eventName,
				Status:     progress.Error,
				StatusText: err.Error(),
			})
		}
		return
	}
	w.Event(progress.CreatedEvent(eventName))
	return
}

func (s *composeService) recreateContainer(ctx context.Context, project *types.Project, service types.ServiceConfig,
	replaced containerType.Summary, inherit bool, timeout *time.Duration,
) (created containerType.Summary, err error) {
	w := progress.ContextWriter(ctx)
	eventName := getContainerProgressName(replaced)
	w.Event(progress.NewEvent(eventName, progress.Working, "Recreate"))
	defer func() {
		if err != nil && ctx.Err() == nil {
			w.Event(progress.Event{
				ID:         eventName,
				Status:     progress.Error,
				StatusText: err.Error(),
			})
		}
	}()

	number, err := strconv.Atoi(replaced.Labels[api.ContainerNumberLabel])
	if err != nil {
		return created, err
	}

	var inherited *containerType.Summary
	if inherit {
		inherited = &replaced
	}

	replacedContainerName := service.ContainerName
	if replacedContainerName == "" {
		replacedContainerName = service.Name + api.Separator + strconv.Itoa(number)
	}
	name := getContainerName(project.Name, service, number)
	tmpName := fmt.Sprintf("%s_%s", replaced.ID[:12], name)
	opts := createOptions{
		AutoRemove:        false,
		AttachStdin:       false,
		UseNetworkAliases: true,
		Labels:            mergeLabels(service.Labels, service.CustomLabels).Add(api.ContainerReplaceLabel, replacedContainerName),
	}
	created, err = s.createMobyContainer(ctx, project, service, tmpName, number, inherited, opts, w)
	if err != nil {
		return created, err
	}

	timeoutInSecond := utils.DurationSecondToInt(timeout)
	err = s.apiClient().ContainerStop(ctx, replaced.ID, containerType.StopOptions{Timeout: timeoutInSecond})
	if err != nil {
		return created, err
	}

	err = s.apiClient().ContainerRemove(ctx, replaced.ID, containerType.RemoveOptions{})
	if err != nil {
		return created, err
	}

	err = s.apiClient().ContainerRename(ctx, tmpName, name)
	if err != nil {
		return created, err
	}

	w.Event(progress.NewEvent(eventName, progress.Done, "Recreated"))
	return created, err
}

// force sequential calls to ContainerStart to prevent race condition in engine assigning ports from ranges
var startMx sync.Mutex

func (s *composeService) startContainer(ctx context.Context, ctr containerType.Summary) error {
	w := progress.ContextWriter(ctx)
	w.Event(progress.NewEvent(getContainerProgressName(ctr), progress.Working, "Restart"))
	startMx.Lock()
	defer startMx.Unlock()
	err := s.apiClient().ContainerStart(ctx, ctr.ID, containerType.StartOptions{})
	if err != nil {
		return err
	}
	w.Event(progress.NewEvent(getContainerProgressName(ctr), progress.Done, "Restarted"))
	return nil
}

func (s *composeService) createMobyContainer(ctx context.Context,
	project *types.Project,
	service types.ServiceConfig,
	name string,
	number int,
	inherit *containerType.Summary,
	opts createOptions,
	w progress.Writer,
) (containerType.Summary, error) {
	var created containerType.Summary
	cfgs, err := s.getCreateConfigs(ctx, project, service, number, inherit, opts)
	if err != nil {
		return created, err
	}
	platform := service.Platform
	if platform == "" {
		platform = project.Environment["DOCKER_DEFAULT_PLATFORM"]
	}
	var plat *specs.Platform
	if platform != "" {
		var p specs.Platform
		p, err = platforms.Parse(platform)
		if err != nil {
			return created, err
		}
		plat = &p
	}

	response, err := s.apiClient().ContainerCreate(ctx, cfgs.Container, cfgs.Host, cfgs.Network, plat, name)
	if err != nil {
		return created, err
	}
	for _, warning := range response.Warnings {
		w.Event(progress.Event{
			ID:     service.Name,
			Status: progress.Warning,
			Text:   warning,
		})
	}
	inspectedContainer, err := s.apiClient().ContainerInspect(ctx, response.ID)
	if err != nil {
		return created, err
	}
	created = containerType.Summary{
		ID:     inspectedContainer.ID,
		Labels: inspectedContainer.Config.Labels,
		Names:  []string{inspectedContainer.Name},
		NetworkSettings: &containerType.NetworkSettingsSummary{
			Networks: inspectedContainer.NetworkSettings.Networks,
		},
	}

	apiVersion, err := s.RuntimeVersion(ctx)
	if err != nil {
		return created, err
	}
	// Starting API version 1.44, the ContainerCreate API call takes multiple networks
	// so we include all the configurations there and can skip the one-by-one calls here
	if versions.LessThan(apiVersion, "1.44") {
		// the highest-priority network is the primary and is included in the ContainerCreate API
		// call via container.NetworkMode & network.NetworkingConfig
		// any remaining networks are connected one-by-one here after creation (but before start)
		serviceNetworks := service.NetworksByPriority()
		for _, networkKey := range serviceNetworks {
			mobyNetworkName := project.Networks[networkKey].Name
			if string(cfgs.Host.NetworkMode) == mobyNetworkName {
				// primary network already configured as part of ContainerCreate
				continue
			}
			epSettings := createEndpointSettings(project, service, number, networkKey, cfgs.Links, opts.UseNetworkAliases)
			if err := s.apiClient().NetworkConnect(ctx, mobyNetworkName, created.ID, epSettings); err != nil {
				return created, err
			}
		}
	}
	return created, nil
}

// getLinks mimics V1 compose/service.py::Service::_get_links()
func (s *composeService) getLinks(ctx context.Context, projectName string, service types.ServiceConfig, number int) ([]string, error) {
	var links []string
	format := func(k, v string) string {
		return fmt.Sprintf("%s:%s", k, v)
	}
	getServiceContainers := func(serviceName string) (Containers, error) {
		return s.getContainers(ctx, projectName, oneOffExclude, true, serviceName)
	}

	for _, rawLink := range service.Links {
		linkSplit := strings.Split(rawLink, ":")
		linkServiceName := linkSplit[0]
		linkName := linkServiceName
		if len(linkSplit) == 2 {
			linkName = linkSplit[1] // linkName if informed like in: "serviceName:linkName"
		}
		cnts, err := getServiceContainers(linkServiceName)
		if err != nil {
			return nil, err
		}
		for _, c := range cnts {
			containerName := getCanonicalContainerName(c)
			links = append(links,
				format(containerName, linkName),
				format(containerName, linkServiceName+api.Separator+strconv.Itoa(number)),
				format(containerName, strings.Join([]string{projectName, linkServiceName, strconv.Itoa(number)}, api.Separator)),
			)
		}
	}

	if service.Labels[api.OneoffLabel] == "True" {
		cnts, err := getServiceContainers(service.Name)
		if err != nil {
			return nil, err
		}
		for _, c := range cnts {
			containerName := getCanonicalContainerName(c)
			links = append(links,
				format(containerName, service.Name),
				format(containerName, strings.TrimPrefix(containerName, projectName+api.Separator)),
				format(containerName, containerName),
			)
		}
	}

	for _, rawExtLink := range service.ExternalLinks {
		extLinkSplit := strings.Split(rawExtLink, ":")
		externalLink := extLinkSplit[0]
		linkName := externalLink
		if len(extLinkSplit) == 2 {
			linkName = extLinkSplit[1]
		}
		links = append(links, format(externalLink, linkName))
	}
	return links, nil
}

func (s *composeService) isServiceHealthy(ctx context.Context, containers Containers, fallbackRunning bool) (bool, error) {
	for _, c := range containers {
		container, err := s.apiClient().ContainerInspect(ctx, c.ID)
		if err != nil {
			return false, err
		}
		name := container.Name[1:]

		if container.State.Status == "exited" {
			return false, fmt.Errorf("container %s exited (%d)", name, container.State.ExitCode)
		}

		if container.Config.Healthcheck == nil && fallbackRunning {
			// Container does not define a health check, but we can fall back to "running" state
			return container.State != nil && container.State.Status == "running", nil
		}

		if container.State == nil || container.State.Health == nil {
			return false, fmt.Errorf("container %s has no healthcheck configured", name)
		}
		switch container.State.Health.Status {
		case containerType.Healthy:
			// Continue by checking the next container.
		case containerType.Unhealthy:
			return false, fmt.Errorf("container %s is unhealthy", name)
		case containerType.Starting:
			return false, nil
		default:
			return false, fmt.Errorf("container %s had unexpected health status %q", name, container.State.Health.Status)
		}
	}
	return true, nil
}

func (s *composeService) isServiceCompleted(ctx context.Context, containers Containers) (bool, int, error) {
	for _, c := range containers {
		container, err := s.apiClient().ContainerInspect(ctx, c.ID)
		if err != nil {
			return false, 0, err
		}
		if container.State != nil && container.State.Status == "exited" {
			return true, container.State.ExitCode, nil
		}
	}
	return false, 0, nil
}

func (s *composeService) startService(ctx context.Context,
	project *types.Project, service types.ServiceConfig,
	containers Containers, listener api.ContainerEventListener,
	timeout time.Duration,
) error {
	if service.Deploy != nil && service.Deploy.Replicas != nil && *service.Deploy.Replicas == 0 {
		return nil
	}

	err := s.waitDependencies(ctx, project, service.Name, service.DependsOn, containers, timeout)
	if err != nil {
		return err
	}

	if len(containers) == 0 {
		if service.GetScale() == 0 {
			return nil
		}
		return fmt.Errorf("service %q has no container to start", service.Name)
	}

	w := progress.ContextWriter(ctx)
	for _, ctr := range containers.filter(isService(service.Name)) {
		if ctr.State == ContainerRunning {
			continue
		}

		err = s.injectSecrets(ctx, project, service, ctr.ID)
		if err != nil {
			return err
		}

		err = s.injectConfigs(ctx, project, service, ctr.ID)
		if err != nil {
			return err
		}

		eventName := getContainerProgressName(ctr)
		w.Event(progress.StartingEvent(eventName))
		err = s.apiClient().ContainerStart(ctx, ctr.ID, containerType.StartOptions{})
		if err != nil {
			return err
		}

		for _, hook := range service.PostStart {
			err = s.runHook(ctx, ctr, service, hook, listener)
			if err != nil {
				return err
			}
		}

		w.Event(progress.StartedEvent(eventName))
	}
	return nil
}

func mergeLabels(ls ...types.Labels) types.Labels {
	merged := types.Labels{}
	for _, l := range ls {
		for k, v := range l {
			merged[k] = v
		}
	}
	return merged
}
