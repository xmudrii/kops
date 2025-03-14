/*
Copyright 2019 The Kubernetes Authors.

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

package awstasks

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/klog/v2"
	"k8s.io/kops/pkg/jsonutils"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraformWriter"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"k8s.io/kops/upup/pkg/fi"
	"k8s.io/kops/upup/pkg/fi/cloudup/awsup"
	"k8s.io/kops/upup/pkg/fi/cloudup/terraform"
)

// +kops:fitask
type SQS struct {
	Name      *string
	Lifecycle fi.Lifecycle

	ARN                    *string
	URL                    *string
	MessageRetentionPeriod int
	Policy                 fi.Resource

	Tags map[string]string
}

var _ fi.CompareWithID = &SQS{}

func (q *SQS) CompareWithID() *string {
	return q.ARN
}

func (q *SQS) Find(c *fi.CloudupContext) (*SQS, error) {
	ctx := c.Context()
	cloud := awsup.GetCloud(c)

	if q.Name == nil {
		return nil, nil
	}

	response, err := cloud.SQS().ListQueues(ctx, &sqs.ListQueuesInput{
		MaxResults:      aws.Int32(2),
		QueueNamePrefix: q.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("error listing SQS queues: %v", err)
	}
	if response == nil || len(response.QueueUrls) == 0 {
		return nil, nil
	}
	if len(response.QueueUrls) != 1 {
		return nil, fmt.Errorf("found multiple SQS queues matching queue name")
	}
	url := response.QueueUrls[0]

	attributes, err := cloud.SQS().GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameMessageRetentionPeriod, sqstypes.QueueAttributeNamePolicy, sqstypes.QueueAttributeNameQueueArn},
		QueueUrl:       aws.String(url),
	})
	if err != nil {
		return nil, fmt.Errorf("error getting SQS queue attributes: %v", err)
	}
	actualPolicy := attributes.Attributes["Policy"]
	actualARN := attributes.Attributes["QueueArn"]
	period, err := strconv.Atoi(attributes.Attributes["MessageRetentionPeriod"])
	if err != nil {
		return nil, fmt.Errorf("error coverting MessageRetentionPeriod to int: %v", err)
	}

	tags, err := cloud.SQS().ListQueueTags(ctx, &sqs.ListQueueTagsInput{
		QueueUrl: aws.String(url),
	})
	if err != nil {
		return nil, fmt.Errorf("error listing SQS queue tags: %v", err)
	}

	// We parse both as JSON; if the json forms are equal we pretend the actual value is the expected value
	if q.Policy != nil {
		expectedPolicy, err := fi.ResourceAsString(q.Policy)
		if err != nil {
			return nil, fmt.Errorf("error reading expected Policy for SQS %q: %v", aws.ToString(q.Name), err)
		}
		expectedJson := make(map[string]interface{})
		err = json.Unmarshal([]byte(expectedPolicy), &expectedJson)
		if err != nil {
			return nil, fmt.Errorf("error parsing expected Policy for SQS %q: %v", aws.ToString(q.Name), err)
		}
		actualJson := make(map[string]interface{})
		err = json.Unmarshal([]byte(actualPolicy), &actualJson)
		if err != nil {
			return nil, fmt.Errorf("error parsing actual Policy for SQS %q: %v", aws.ToString(q.Name), err)
		}

		if err := normalizePolicy(expectedJson); err != nil {
			return nil, err
		}
		if err := normalizePolicy(actualJson); err != nil {
			return nil, err
		}

		if reflect.DeepEqual(actualJson, expectedJson) {
			klog.V(2).Infof("actual Policy was json-equal to expected; returning expected value")
			actualPolicy = expectedPolicy
			q.Policy = fi.NewStringResource(expectedPolicy)
		}

	}

	actual := &SQS{
		ARN:                    s(actualARN),
		Name:                   q.Name,
		URL:                    aws.String(url),
		Lifecycle:              q.Lifecycle,
		Policy:                 fi.NewStringResource(actualPolicy),
		MessageRetentionPeriod: period,
		Tags:                   intersectSQSTags(tags.Tags, q.Tags),
	}

	// Avoid flapping
	q.ARN = actual.ARN

	return actual, nil
}

type JSONObject map[string]any

func (j *JSONObject) Slice(key string) ([]any, bool, error) {
	v, found := (*j)[key]
	if !found {
		return nil, false, nil
	}
	s, ok := v.([]any)
	if !ok {
		return nil, false, fmt.Errorf("expected slice at %q, got %T", key, v)
	}
	return s, true, nil
}

func (j *JSONObject) Object(key string) (JSONObject, bool, error) {
	v, found := (*j)[key]
	if !found {
		return nil, false, nil
	}
	m, ok := v.(JSONObject)
	if !ok {
		return nil, false, fmt.Errorf("expected object at %q, got %T", key, v)
	}
	return m, true, nil
}

// normalizePolicy sorts the Service principals in the policy, so that we can compare policies more easily.
func normalizePolicy(policy map[string]interface{}) error {
	xform := jsonutils.NewTransformer()
	xform.AddSliceTransform(func(path string, value []any) ([]any, error) {
		if path != ".Statement[].Principal.Service[]" {
			return value, nil
		}
		return jsonutils.SortSlice(value)
	})
	if err := xform.Transform(policy); err != nil {
		return err
	}
	return nil
}

func (q *SQS) Run(c *fi.CloudupContext) error {
	return fi.CloudupDefaultDeltaRunMethod(q, c)
}

func (q *SQS) CheckChanges(a, e, changes *SQS) error {
	if a == nil {
		if e.Name == nil {
			return field.Required(field.NewPath("Name"), "")
		}
	}
	if a != nil {
		if changes.URL != nil {
			return fi.CannotChangeField("URL")
		}
	}
	return nil
}

func (q *SQS) RenderAWS(t *awsup.AWSAPITarget, a, e, changes *SQS) error {
	ctx := context.TODO()
	policy, err := fi.ResourceAsString(e.Policy)
	if err != nil {
		return fmt.Errorf("error rendering RolePolicyDocument: %v", err)
	}

	if a == nil {
		request := &sqs.CreateQueueInput{
			Attributes: map[string]string{
				"MessageRetentionPeriod": strconv.Itoa(q.MessageRetentionPeriod),
				"Policy":                 policy,
			},
			QueueName: q.Name,
			Tags:      q.Tags,
		}
		response, err := t.Cloud.SQS().CreateQueue(ctx, request)
		if err != nil {
			return fmt.Errorf("error creating SQS queue: %v", err)
		}

		attributes, err := t.Cloud.SQS().GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn},
			QueueUrl:       response.QueueUrl,
		})
		if err != nil {
			return fmt.Errorf("error getting SQS queue attributes: %v", err)
		}

		e.ARN = aws.String(attributes.Attributes["QueueArn"])
	}

	return nil
}

type terraformSQSQueue struct {
	Name                    *string                  `cty:"name"`
	MessageRetentionSeconds int                      `cty:"message_retention_seconds"`
	Policy                  *terraformWriter.Literal `cty:"policy"`
	Tags                    map[string]string        `cty:"tags"`
}

func (_ *SQS) RenderTerraform(t *terraform.TerraformTarget, a, e, changes *SQS) error {
	p, err := t.AddFileResource("aws_sqs_queue", *e.Name, "policy", e.Policy, false)
	if err != nil {
		return err
	}

	tf := &terraformSQSQueue{
		Name:                    e.Name,
		MessageRetentionSeconds: e.MessageRetentionPeriod,
		Policy:                  p,
		Tags:                    e.Tags,
	}

	return t.RenderResource("aws_sqs_queue", *e.Name, tf)
}

func (e *SQS) TerraformLink() *terraformWriter.Literal {
	return terraformWriter.LiteralProperty("aws_sqs_queue", *e.Name, "arn")
}

// intersectSQSTags does the same thing as intersectTags, but takes different input because SQS tags are listed differently
func intersectSQSTags(tags map[string]string, desired map[string]string) map[string]string {
	if tags == nil {
		return nil
	}
	actual := make(map[string]string)
	for k, v := range tags {
		if _, found := desired[k]; found {
			actual[k] = v
		}
	}
	if len(actual) == 0 && desired == nil {
		// Avoid problems with comparison between nil & {}
		return nil
	}
	return actual
}
