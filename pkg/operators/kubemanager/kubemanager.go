// Copyright 2023-2025 The Inspektor Gadget authors
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

package kubemanager

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cilium/ebpf"
	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"

	containercollection "github.com/inspektor-gadget/inspektor-gadget/pkg/container-collection"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/datasource"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/datasource/compat"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/gadget-service/api"
	apihelpers "github.com/inspektor-gadget/inspektor-gadget/pkg/gadget-service/api-helpers"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/gadgets"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/gadgettracermanager"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/logger"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/operators"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/operators/common"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/params"
	"github.com/inspektor-gadget/inspektor-gadget/pkg/types"
)

const (
	OperatorName       = "KubeManager"
	ParamContainerName = "containername"
	ParamSelector      = "selector"
	ParamAllNamespaces = "all-namespaces"
	ParamPodName       = "podname"
	ParamNamespace     = "namespace"
)

type MountNsMapSetter interface {
	SetMountNsMap(*ebpf.Map)
}

type Attacher interface {
	AttachContainer(container *containercollection.Container) error
	DetachContainer(*containercollection.Container) error
}

type KubeManager struct {
	gadgetTracerManager *gadgettracermanager.GadgetTracerManager
}

func (k *KubeManager) SetGadgetTracerMgr(g *gadgettracermanager.GadgetTracerManager) {
	log.Infof("gadget tracermgr set in kubemanager")
	k.gadgetTracerManager = g
}

func (k *KubeManager) Name() string {
	return OperatorName
}

func (k *KubeManager) Description() string {
	return "KubeManager handles container/pod/namespace information using Container-Collection and GadgetTracerMgr"
}

func (k *KubeManager) GlobalParamDescs() params.ParamDescs {
	return nil
}

func (k *KubeManager) ParamDescs() params.ParamDescs {
	return params.ParamDescs{
		{
			Key:         ParamContainerName,
			Alias:       "c",
			Description: "Show only data from containers with that name",
			ValueHint:   gadgets.K8SContainerName,
		},
		{
			Key:         ParamSelector,
			Alias:       "l",
			Description: "Labels selector to filter on. Only '=' is supported (e.g. key1=value1,key2=value2).",
			ValueHint:   gadgets.K8SLabels,
			Validator: func(value string) error {
				if value == "" {
					return nil
				}

				pairs := strings.Split(value, ",")
				for _, pair := range pairs {
					kv := strings.Split(pair, "=")
					if len(kv) != 2 {
						return fmt.Errorf("should be a comma-separated list of key-value pairs (key=value[,key=value,...])")
					}
				}

				return nil
			},
		},
		{
			Key:         ParamPodName,
			Alias:       "p",
			Description: "Show only data from pods with that name",
			ValueHint:   gadgets.K8SPodName,
		},
		{
			Key:          ParamAllNamespaces,
			Alias:        "A",
			Description:  "Show data from pods in all namespaces",
			TypeHint:     params.TypeBool,
			DefaultValue: "false",
		},
		{
			Key:         ParamNamespace,
			Alias:       "n",
			Description: "Show only data from pods in a given namespace",
			ValueHint:   gadgets.K8SNamespace,
		},
	}
}

func (k *KubeManager) Init(params *params.Params) error {
	return nil
}

func (k *KubeManager) Close() error {
	return nil
}

type KubeManagerInstance struct {
	id           string
	manager      *KubeManager
	enrichEvents bool
	mountnsmap   *ebpf.Map
	subscribed   bool

	attachedContainers map[string]*containercollection.Container
	attacher           Attacher
	params             *params.Params
	gadgetInstance     any
	gadgetCtx          operators.GadgetContext

	eventWrappers map[datasource.DataSource]*compat.EventWrapperBase

	containersPublisher *common.ContainersPublisher
}

func (m *KubeManagerInstance) Name() string {
	return OperatorName
}

func newContainerSelector(selectorSlice []string, namespace, podName, containerName string, useAllNamespace bool) containercollection.ContainerSelector {
	labels := make(map[string]string)
	for _, pair := range selectorSlice {
		kv := strings.Split(pair, "=")
		labels[kv[0]] = kv[1]
	}

	containerSelector := containercollection.ContainerSelector{
		K8s: containercollection.K8sSelector{
			BasicK8sMetadata: types.BasicK8sMetadata{
				Namespace:     namespace,
				PodName:       podName,
				ContainerName: containerName,
				PodLabels:     labels,
			},
		},
	}

	if useAllNamespace {
		containerSelector.K8s.Namespace = ""
	}

	return containerSelector
}

func (m *KubeManagerInstance) PreGadgetRun() error {
	log := m.gadgetCtx.Logger()

	if m.gadgetInstance != nil {
		err := m.handleGadgetInstance(log)
		if err != nil {
			return err
		}
	}

	return nil
}

func (m *KubeManagerInstance) handleGadgetInstance(log logger.Logger) error {
	containerSelector := newContainerSelector(
		m.params.Get(ParamSelector).AsStringSlice(),
		m.params.Get(ParamNamespace).AsString(),
		m.params.Get(ParamPodName).AsString(),
		m.params.Get(ParamContainerName).AsString(),
		m.params.Get(ParamAllNamespaces).AsBool(),
	)

	if setter, ok := m.gadgetInstance.(MountNsMapSetter); ok {
		err := m.manager.gadgetTracerManager.AddTracer(m.id, containerSelector)
		if err != nil {
			return fmt.Errorf("adding tracer: %w", err)
		}

		// Create mount namespace map to filter by containers
		mountnsmap, err := m.manager.gadgetTracerManager.TracerMountNsMap(m.id)
		if err != nil {
			m.manager.gadgetTracerManager.RemoveTracer(m.id)
			return fmt.Errorf("creating mountns map: %w", err)
		}

		log.Debugf("set mountnsmap for gadget")
		setter.SetMountNsMap(mountnsmap)

		m.mountnsmap = mountnsmap
	}

	if attacher, ok := m.gadgetInstance.(Attacher); ok {
		m.attacher = attacher
		m.attachedContainers = make(map[string]*containercollection.Container)

		attachContainerFunc := func(container *containercollection.Container) {
			log.Debugf("calling gadget.AttachContainer()")
			err := attacher.AttachContainer(container)
			if err != nil {
				var ve *ebpf.VerifierError
				if errors.As(err, &ve) {
					m.gadgetCtx.Logger().Debugf("start tracing container %q: verifier error: %+v\n", container.K8s.ContainerName, ve)
				}

				log.Warnf("start tracing container %q: %s", container.K8s.ContainerName, err)
				return
			}

			m.attachedContainers[container.Runtime.ContainerID] = container

			log.Debugf("tracer attached: container %q pid %d mntns %d netns %d",
				container.K8s.ContainerName, container.ContainerPid(), container.Mntns, container.Netns)
		}

		detachContainerFunc := func(container *containercollection.Container) {
			log.Debugf("calling gadget.Detach()")
			delete(m.attachedContainers, container.Runtime.ContainerID)

			err := attacher.DetachContainer(container)
			if err != nil {
				log.Warnf("stop tracing container %q: %s", container.K8s.ContainerName, err)
				return
			}
			log.Debugf("tracer detached: container %q pid %d mntns %d netns %d",
				container.K8s.ContainerName, container.ContainerPid(), container.Mntns, container.Netns)
		}

		m.subscribed = true

		log.Debugf("add subscription to gadgetTracerManager")
		containers := m.manager.gadgetTracerManager.Subscribe(
			m.id,
			containerSelector,
			func(event containercollection.PubSubEvent) {
				log.Debugf("%s: %s", event.Type.String(), event.Container.Runtime.ContainerID)
				switch event.Type {
				case containercollection.EventTypeAddContainer:
					attachContainerFunc(event.Container)
				case containercollection.EventTypeRemoveContainer:
					detachContainerFunc(event.Container)
				case containercollection.EventTypePreCreateContainer:
					// nothing to do
				default:
					log.Errorf("unknown event type, expected either %s, %s or %s, got %s",
						containercollection.EventTypePreCreateContainer,
						containercollection.EventTypeAddContainer,
						containercollection.EventTypeRemoveContainer,
						event.Type)
				}
			},
		)

		for _, container := range containers {
			attachContainerFunc(container)
		}
	}
	return nil
}

func (m *KubeManagerInstance) PostGadgetRun() error {
	if m.mountnsmap != nil {
		m.gadgetCtx.Logger().Debugf("calling RemoveTracer()")
		m.manager.gadgetTracerManager.RemoveTracer(m.id)
	}

	if m.subscribed {
		m.gadgetCtx.Logger().Debugf("calling Unsubscribe()")
		m.manager.gadgetTracerManager.Unsubscribe(m.id)

		// emit detach for all remaining containers
		for _, container := range m.attachedContainers {
			m.attacher.DetachContainer(container)
		}
	}

	return nil
}

func (m *KubeManagerInstance) enrich(ev any) {
	if event, canEnrichEventFromMountNs := ev.(operators.ContainerInfoFromMountNSID); canEnrichEventFromMountNs {
		m.manager.gadgetTracerManager.EnrichEventByMntNs(event)
	}
	if event, canEnrichEventFromNetNs := ev.(operators.ContainerInfoFromNetNSID); canEnrichEventFromNetNs {
		m.manager.gadgetTracerManager.EnrichEventByNetNs(event)
	}
}

func (m *KubeManagerInstance) EnrichEvent(ev any) error {
	if !m.enrichEvents {
		return nil
	}
	m.enrich(ev)
	return nil
}

func (k *KubeManager) GlobalParams() api.Params {
	return apihelpers.ParamDescsToParams(k.GlobalParamDescs())
}

func (k *KubeManager) InstanceParams() api.Params {
	return apihelpers.ParamDescsToParams(k.ParamDescs())
}

func (k *KubeManager) InstantiateDataOperator(gadgetCtx operators.GadgetContext, paramValues api.ParamValues) (
	operators.DataOperatorInstance, error,
) {
	params := k.ParamDescs().ToParams()
	err := params.CopyFromMap(paramValues, "")
	if err != nil {
		return nil, err
	}

	cfg, ok := gadgetCtx.GetVar("config")
	if !ok {
		return nil, fmt.Errorf("missing configuration")
	}
	v, ok := cfg.(*viper.Viper)
	if !ok {
		return nil, fmt.Errorf("invalid configuration format")
	}

	enableContainersDs := v.GetBool("annotations.enable-containers-datasource")

	var containersPublisher *common.ContainersPublisher
	if enableContainersDs {
		containersPublisher, err = common.NewContainersPublisher(gadgetCtx, &k.gadgetTracerManager.ContainerCollection)
		if err != nil {
			return nil, fmt.Errorf("creating containers publisher: %w", err)
		}
	}

	traceInstance := &KubeManagerInstance{
		manager:            k,
		enrichEvents:       false,
		attachedContainers: make(map[string]*containercollection.Container),
		params:             params,
		gadgetCtx:          gadgetCtx,
		id:                 uuid.New().String(),

		eventWrappers: make(map[datasource.DataSource]*compat.EventWrapperBase),

		containersPublisher: containersPublisher,
	}

	activate := false

	// Check, whether the gadget requested a map from us
	if t, ok := gadgetCtx.GetVar(gadgets.MntNsFilterMapName); ok {
		if _, ok := t.(*ebpf.Map); ok {
			gadgetCtx.Logger().Debugf("gadget requested map %s", gadgets.MntNsFilterMapName)
			activate = true
		}
	}

	// Check for NeedContainerEvents; this is set for example for tchandlers, as they
	// require the Attacher interface to be aware of containers
	if val, ok := gadgetCtx.GetVar("NeedContainerEvents"); ok {
		if b, ok := val.(bool); ok && b {
			activate = true
		}
	}

	wrappers, err := compat.GetEventWrappers(gadgetCtx)
	if err != nil {
		return nil, fmt.Errorf("getting event wrappers: %w", err)
	}
	traceInstance.eventWrappers = wrappers
	if len(wrappers) > 0 {
		activate = true
	}

	if !activate {
		return nil, nil
	}

	return traceInstance, nil
}

func (k *KubeManager) Priority() int {
	return -1
}

func (m *KubeManagerInstance) ParamDescs(gadgetCtx operators.GadgetContext) params.ParamDescs {
	return m.manager.ParamDescs()
}

func (m *KubeManagerInstance) PreStart(gadgetCtx operators.GadgetContext) error {
	m.gadgetInstance, _ = gadgetCtx.GetVar("ebpfInstance")

	compat.Subscribe(
		m.eventWrappers,
		m.manager.gadgetTracerManager.EnrichEventByMntNs,
		m.manager.gadgetTracerManager.EnrichEventByNetNs,
		0,
	)

	labels := make(map[string]string)
	selectorSlice := m.params.Get(ParamSelector).AsStringSlice()
	for _, pair := range selectorSlice {
		kv := strings.Split(pair, "=")
		labels[kv[0]] = kv[1]
	}

	containerSelector := containercollection.ContainerSelector{
		K8s: containercollection.K8sSelector{
			BasicK8sMetadata: types.BasicK8sMetadata{
				Namespace:     m.params.Get(ParamNamespace).AsString(),
				PodName:       m.params.Get(ParamPodName).AsString(),
				ContainerName: m.params.Get(ParamContainerName).AsString(),
				PodLabels:     labels,
			},
		},
	}

	if m.params.Get(ParamAllNamespaces).AsBool() {
		containerSelector.K8s.Namespace = ""
	}

	if m.manager.gadgetTracerManager == nil {
		return fmt.Errorf("container-collection isn't available")
	}

	// Create mount namespace map to filter by containers
	err := m.manager.gadgetTracerManager.AddTracer(m.id, containerSelector)
	if err != nil {
		return fmt.Errorf("adding tracer: %w", err)
	}

	mountnsmap, err := m.manager.gadgetTracerManager.TracerMountNsMap(m.id)
	if err != nil {
		m.manager.gadgetTracerManager.RemoveTracer(m.id)
		return fmt.Errorf("creating mountnsmap: %w", err)
	}

	gadgetCtx.Logger().Debugf("set mountnsmap for gadget")
	gadgetCtx.SetVar(gadgets.MntNsFilterMapName, mountnsmap)
	gadgetCtx.SetVar(gadgets.FilterByMntNsName, true)

	m.mountnsmap = mountnsmap
	// using PreGadgetRun() for the time being to register attacher funcs
	return m.PreGadgetRun()
}

func (m *KubeManagerInstance) Start(gadgetCtx operators.GadgetContext) error {
	if m.containersPublisher == nil {
		return nil
	}

	containerSelector := newContainerSelector(
		m.params.Get(ParamSelector).AsStringSlice(),
		m.params.Get(ParamNamespace).AsString(),
		m.params.Get(ParamPodName).AsString(),
		m.params.Get(ParamContainerName).AsString(),
		m.params.Get(ParamAllNamespaces).AsBool(),
	)

	return m.containersPublisher.PublishContainers(true, []*containercollection.Container{}, containerSelector)
}

func (m *KubeManagerInstance) Stop(gadgetCtx operators.GadgetContext) error {
	return nil
}

func (m *KubeManagerInstance) Close(gadgetCtx operators.GadgetContext) error {
	m.manager.gadgetTracerManager.RemoveTracer(m.id)

	if m.containersPublisher != nil {
		m.containersPublisher.Unsubscribe()
	}

	return nil
}

var KubeManagerOperator *KubeManager

func init() {
	KubeManagerOperator = &KubeManager{}
	operators.RegisterDataOperator(KubeManagerOperator)
}
