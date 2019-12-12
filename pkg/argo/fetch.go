// Copyright 2019 The SQLFlow Authors. All rights reserved.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package argo

import (
	"fmt"
	wfv1 "github.com/argoproj/argo/pkg/apis/workflow/v1alpha1"
	"os/exec"
	pb "sqlflow.org/sqlflow/pkg/proto"
	"time"
)

// Fetch fetches the workflow log and status
//
// if token.step_id == "" {
//    NOTE(Yancey): wait mean wait for Running
//    my_step := first step
// } else {
//    my_step := next(token.step_id)
// }
//
// if my_step is pending/running, return ""
// if my_step is complete, return (logs, my_step_id)
func Fetch(token pb.FetchToken) (*pb.FetchResponse, error) {
	// FIXME(tony): no need to wait for the whole workflow
	wf, err := waitUntilComplete(token)
	if err != nil {
		return nil, err
	}

	stepGroupName, err := getCurrentStepGroup(wf, token)
	if err != nil {
		return nil, err
	}
	// End of fetching, no more logs
	if stepGroupName == "" {
		return &pb.FetchResponse{
			NewToken: &pb.FetchToken{
				Job:       token.Job,
				StepId:    stepGroupName,
				LogOffset: "",
				NoMoreLog: true},
			Logs:  &pb.FetchResponse_Logs{},
			Phase: translatePhase(wf.Status.Phase)}, nil
	}

	podName, err := getCurrentPodName(wf, token)
	if err != nil {
		return nil, err
	}

	logs, err := getPodLogs(podName)
	if err != nil {
		return nil, err
	}

	// TODO(yancey&tony): update the following constant after supporting incremental fetching
	logOffset := ""
	finishedFetchingCurrentPod := true

	var newStepGroupName string
	if finishedFetchingCurrentPod {
		newStepGroupName = stepGroupName
	} else {
		newStepGroupName = token.StepId
	}

	return &pb.FetchResponse{
		NewToken: &pb.FetchToken{
			Job:       token.Job,
			StepId:    newStepGroupName,
			LogOffset: logOffset,
			NoMoreLog: false},
		Logs:  &pb.FetchResponse_Logs{Content: []string{logs}},
		Phase: translatePhase(wf.Status.Phase)}, nil
}

// NewFetchToken creates a fetch token
func NewFetchToken(job pb.Job) pb.FetchToken {
	return pb.FetchToken{
		Job:       &job,
		StepId:    "",
		LogOffset: "",
		NoMoreLog: false}
}

func getWorkflowResource(token pb.FetchToken) (*wfv1.Workflow, error) {
	cmd := exec.Command("kubectl", "get", "wf", token.Job.Id, "-o", "json")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("getWorkflowResource error: %v\n%v", string(output), err)
	}
	return parseWorkflowResource(output)
}

func checkNodeType(expected, actual wfv1.NodeType) error {
	if expected != actual {
		return fmt.Errorf("checkNodeType failed %v(expected) != %v(actual)", expected, actual)
	}
	return nil
}

func getPodLogs(podName string) (string, error) {
	// NOTE(tony): A workflow pod usually contains two container: main and wait
	// I believe wait is used for management by Argo, so we only need to care about main.
	cmd := exec.Command("kubectl", "logs", podName, "main")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("getPodLogs error: %v\n%v", string(output), err)
	}
	return string(output), nil
}

func waitUntilComplete(token pb.FetchToken) (wf *wfv1.Workflow, err error) {
	for {
		wf, err = getWorkflowResource(token)
		if err != nil {
			return nil, fmt.Errorf("waitUntilComplete: %v", err)
		}
		if isCompletedPhaseWF(wf.Status.Phase) {
			return wf, nil
		}
		time.Sleep(time.Second)
	}
}

func getPodNameByStepGroup(wf *wfv1.Workflow, stepGroupName string) (string, error) {
	stepGroupNode, ok := wf.Status.Nodes[stepGroupName]
	if !ok {
		return "", fmt.Errorf("getPodNameByStepGroup: stepGroup %v doesn't exist", stepGroupName)
	}
	if err := checkNodeType(wfv1.NodeTypeStepGroup, stepGroupNode.Type); err != nil {
		return "", fmt.Errorf("getPodNameByStepGroup: %v", err)
	}
	if l := len(stepGroupNode.Children); l != 1 {
		return "", fmt.Errorf("getPodNameByStepGroup: unexpected len(stepGroupNode.Children) 1 != %v", l)
	}
	return stepGroupNode.Children[0], nil
}

func getNextStepGroup(wf *wfv1.Workflow, current string) (string, error) {
	stepGroupNode := wf.Status.Nodes[current]
	if err := checkNodeType(wfv1.NodeTypeStepGroup, stepGroupNode.Type); err != nil {
		return "", fmt.Errorf("getNextStepGroup: %v", err)
	}
	if l := len(stepGroupNode.Children); l != 1 {
		return "", fmt.Errorf("getNextStepGroup: unexpected len(stepGroupNode.Children) 1 != %v", l)
	}
	podNode := wf.Status.Nodes[stepGroupNode.Children[0]]
	if err := checkNodeType(wfv1.NodeTypePod, podNode.Type); err != nil {
		return "", fmt.Errorf("getNextStepGroup %v", err)
	}

	if len(podNode.Children) == 0 {
		return "", nil
	}
	if l := len(podNode.Children); l != 1 {
		return "", fmt.Errorf("getNextStepGroup: unexpected len(podNode.Children) 1 != %v", l)
	}
	return podNode.Children[0], nil
}

func getCurrentStepGroup(wf *wfv1.Workflow, token pb.FetchToken) (string, error) {
	if token.StepId == "" {
		stepNode := wf.Status.Nodes[token.Job.Id]
		if err := checkNodeType(wfv1.NodeTypeSteps, stepNode.Type); err != nil {
			return "", fmt.Errorf("getCurrentStepGroup: %v", err)
		}
		if l := len(stepNode.Children); l != 1 {
			return "", fmt.Errorf("getCurrentStepGroup: unexpected len(stepNode.Children) 1 != %v", l)
		}
		return stepNode.Children[0], nil
	}
	return getNextStepGroup(wf, token.StepId)
}

func getCurrentPodName(wf *wfv1.Workflow, token pb.FetchToken) (string, error) {
	if err := checkNodeType(wfv1.NodeTypeSteps, wf.Status.Nodes[token.Job.Id].Type); err != nil {
		return "", fmt.Errorf("getPodNameByStepId error: %v", err)
	}

	stepGroupName, err := getCurrentStepGroup(wf, token)
	if err != nil {
		return "", err
	}
	if stepGroupName == "" {
		return "", nil
	}

	return getPodNameByStepGroup(wf, stepGroupName)
}