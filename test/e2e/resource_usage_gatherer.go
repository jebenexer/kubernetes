/*
Copyright 2015 The Kubernetes Authors All rights reserved.

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

package e2e

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	. "github.com/onsi/gomega"
	"k8s.io/kubernetes/pkg/api"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	utilruntime "k8s.io/kubernetes/pkg/util/runtime"
)

const (
	resourceDataGatheringPeriod = 60 * time.Second
)

type resourceConstraint struct {
	cpuConstraint    float64
	memoryConstraint uint64
}

type SingleContainerSummary struct {
	Name string
	Cpu  float64
	Mem  uint64
}

// we can't have int here, as JSON does not accept integer keys.
type ResourceUsageSummary map[string][]SingleContainerSummary

func (s *ResourceUsageSummary) PrintHumanReadable() string {
	buf := &bytes.Buffer{}
	w := tabwriter.NewWriter(buf, 1, 0, 1, ' ', 0)
	for perc, summaries := range *s {
		buf.WriteString(fmt.Sprintf("%v percentile:\n", perc))
		fmt.Fprintf(w, "container\tcpu(cores)\tmemory(MB)\n")
		for _, summary := range summaries {
			fmt.Fprintf(w, "%q\t%.3f\t%.2f\n", summary.Name, summary.Cpu, float64(summary.Mem)/(1024*1024))
		}
		w.Flush()
	}
	return buf.String()
}

func (s *ResourceUsageSummary) PrintJSON() string {
	return prettyPrintJSON(*s)
}

func computePercentiles(timeSeries []resourceUsagePerContainer, percentilesToCompute []int) map[int]resourceUsagePerContainer {
	if len(timeSeries) == 0 {
		return make(map[int]resourceUsagePerContainer)
	}
	dataMap := make(map[string]*usageDataPerContainer)
	for i := range timeSeries {
		for name, data := range timeSeries[i] {
			if dataMap[name] == nil {
				dataMap[name] = &usageDataPerContainer{
					cpuData:        make([]float64, len(timeSeries)),
					memUseData:     make([]uint64, len(timeSeries)),
					memWorkSetData: make([]uint64, len(timeSeries)),
				}
			}
			dataMap[name].cpuData = append(dataMap[name].cpuData, data.CPUUsageInCores)
			dataMap[name].memUseData = append(dataMap[name].memUseData, data.MemoryUsageInBytes)
			dataMap[name].memWorkSetData = append(dataMap[name].memWorkSetData, data.MemoryWorkingSetInBytes)
		}
	}
	for _, v := range dataMap {
		sort.Float64s(v.cpuData)
		sort.Sort(uint64arr(v.memUseData))
		sort.Sort(uint64arr(v.memWorkSetData))
	}

	result := make(map[int]resourceUsagePerContainer)
	for _, perc := range percentilesToCompute {
		data := make(resourceUsagePerContainer)
		for k, v := range dataMap {
			percentileIndex := int(math.Ceil(float64(len(v.cpuData)*perc)/100)) - 1
			data[k] = &containerResourceUsage{
				Name:                    k,
				CPUUsageInCores:         v.cpuData[percentileIndex],
				MemoryUsageInBytes:      v.memUseData[percentileIndex],
				MemoryWorkingSetInBytes: v.memWorkSetData[percentileIndex],
			}
		}
		result[perc] = data
	}
	return result
}

func leftMergeData(left, right map[int]resourceUsagePerContainer) map[int]resourceUsagePerContainer {
	result := make(map[int]resourceUsagePerContainer)
	for percentile, data := range left {
		result[percentile] = data
		if _, ok := right[percentile]; !ok {
			continue
		}
		for k, v := range right[percentile] {
			result[percentile][k] = v
		}
	}
	return result
}

type resourceGatherWorker struct {
	c                    *client.Client
	nodeName             string
	wg                   *sync.WaitGroup
	containerIDToNameMap map[string]string
	containerIDs         []string
	stopCh               chan struct{}
	dataSeries           []resourceUsagePerContainer
}

func (w *resourceGatherWorker) singleProbe() {
	data := make(resourceUsagePerContainer)
	nodeUsage, err := getOneTimeResourceUsageOnNode(w.c, w.nodeName, 15*time.Second, func() []string { return w.containerIDs }, true)
	if err != nil {
		Logf("Error while reading data from %v: %v", w.nodeName, err)
		return
	}
	for k, v := range nodeUsage {
		data[w.containerIDToNameMap[k]] = v
	}
	w.dataSeries = append(w.dataSeries, data)
}

func (w *resourceGatherWorker) gather(initialSleep time.Duration) {
	defer utilruntime.HandleCrash()
	defer w.wg.Done()
	select {
	case <-time.After(initialSleep):
		w.singleProbe()
		for {
			select {
			case <-time.After(resourceDataGatheringPeriod):
				w.singleProbe()
			case <-w.stopCh:
				return
			}
		}
	case <-w.stopCh:
		return
	}
}

func (g *containerResourceGatherer) getKubeSystemContainersResourceUsage(c *client.Client) {
	delay := resourceDataGatheringPeriod / time.Duration(len(g.workers))
	for i := range g.workers {
		go g.workers[i].gather(delay)
	}
	g.workerWg.Wait()
}

type containerResourceGatherer struct {
	client               *client.Client
	stopCh               chan struct{}
	workers              []resourceGatherWorker
	workerWg             sync.WaitGroup
	containerIDToNameMap map[string]string
	containerIDs         []string
}

func NewResourceUsageGatherer(c *client.Client) (*containerResourceGatherer, error) {
	g := containerResourceGatherer{
		client:               c,
		stopCh:               make(chan struct{}),
		containerIDToNameMap: make(map[string]string),
		containerIDs:         make([]string, 0),
	}

	pods, err := c.Pods("kube-system").List(api.ListOptions{})
	if err != nil {
		Logf("Error while listing Pods: %v", err)
		return nil, err
	}
	for _, pod := range pods.Items {
		for _, container := range pod.Status.ContainerStatuses {
			containerID := strings.TrimPrefix(container.ContainerID, "docker:/")
			g.containerIDToNameMap[containerID] = pod.Name + "/" + container.Name
			g.containerIDs = append(g.containerIDs, containerID)
		}
	}
	nodeList, err := c.Nodes().List(api.ListOptions{})
	if err != nil {
		Logf("Error while listing Nodes: %v", err)
		return nil, err
	}

	g.workerWg.Add(len(nodeList.Items))
	for _, node := range nodeList.Items {
		g.workers = append(g.workers, resourceGatherWorker{
			c:                    c,
			nodeName:             node.Name,
			wg:                   &g.workerWg,
			containerIDToNameMap: g.containerIDToNameMap,
			containerIDs:         g.containerIDs,
			stopCh:               g.stopCh,
		})
	}
	return &g, nil
}

// startGatheringData blocks until stopAndSummarize is called.
func (g *containerResourceGatherer) startGatheringData() {
	g.getKubeSystemContainersResourceUsage(g.client)
}

func (g *containerResourceGatherer) stopAndSummarize(percentiles []int, constraints map[string]resourceConstraint) *ResourceUsageSummary {
	close(g.stopCh)
	Logf("Closed stop channel. Waiting for %v workers", len(g.workers))
	g.workerWg.Wait()
	Logf("Waitgroup finished.")
	if len(percentiles) == 0 {
		Logf("Warning! Empty percentile list for stopAndPrintData.")
		return &ResourceUsageSummary{}
	}
	data := make(map[int]resourceUsagePerContainer)
	for i := range g.workers {
		stats := computePercentiles(g.workers[i].dataSeries, percentiles)
		data = leftMergeData(stats, data)
	}

	// Workers has been stopped. We need to gather data stored in them.
	sortedKeys := []string{}
	for name := range data[percentiles[0]] {
		sortedKeys = append(sortedKeys, name)
	}
	sort.Strings(sortedKeys)
	violatedConstraints := make([]string, 0)
	summary := make(ResourceUsageSummary)
	for _, perc := range percentiles {
		for _, name := range sortedKeys {
			usage := data[perc][name]
			summary[strconv.Itoa(perc)] = append(summary[strconv.Itoa(perc)], SingleContainerSummary{
				Name: name,
				Cpu:  usage.CPUUsageInCores,
				Mem:  usage.MemoryWorkingSetInBytes,
			})
			// Verifying 99th percentile of resource usage
			if perc == 99 {
				// Name has a form: <pod_name>/<container_name>
				containerName := strings.Split(name, "/")[1]
				if constraint, ok := constraints[containerName]; ok {
					if usage.CPUUsageInCores > constraint.cpuConstraint {
						violatedConstraints = append(
							violatedConstraints,
							fmt.Sprintf("Container %v is using %v/%v CPU",
								name,
								usage.CPUUsageInCores,
								constraint.cpuConstraint,
							),
						)
					}
					if usage.MemoryWorkingSetInBytes > constraint.memoryConstraint {
						violatedConstraints = append(
							violatedConstraints,
							fmt.Sprintf("Container %v is using %v/%v MB of memory",
								name,
								float64(usage.MemoryWorkingSetInBytes)/(1024*1024),
								float64(constraint.memoryConstraint)/(1024*1024),
							),
						)
					}
				}
			}
		}
	}
	Expect(violatedConstraints).To(BeEmpty())
	return &summary
}
