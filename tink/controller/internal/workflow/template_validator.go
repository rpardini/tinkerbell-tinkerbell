package workflow

import (
	"bytes"
	"errors"
	"fmt"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"github.com/distribution/reference"
	"gopkg.in/yaml.v3"
)

const (
	errInvalidLength   = "name cannot be empty or have more than 200 characters: %s"
	errTemplateParsing = "failed to parse template with ID %s"
)

// parse parses the template yaml content into a Workflow.
func parse(yamlContent []byte) (*Workflow, error) {
	var workflow Workflow
	if err := yaml.Unmarshal(yamlContent, &workflow); err != nil {
		// The yamlContent is normally quite large but is invaluable in debugging.
		return &Workflow{}, fmt.Errorf("parsing yaml data: err: %w, content: %s", err, yamlContent)
	}

	if err := validate(&workflow); err != nil {
		return &Workflow{}, fmt.Errorf("validating workflow template: %w", err)
	}

	return &workflow, nil
}

// renderTemplateHardware renders the workflow template and returns the Workflow and the interpolated bytes.
func renderTemplateHardware(templateID, templateData string, hardware map[string]interface{}) (*Workflow, error) {
	t := template.New("workflow-template").
		Option("missingkey=error").
		Funcs(sprig.FuncMap()).
		Funcs(templateFuncs)

	_, err := t.Parse(templateData)
	if err != nil {
		err = fmt.Errorf("%s: err: %w", fmt.Sprintf(errTemplateParsing, templateID), err)
		return nil, err
	}

	var buf bytes.Buffer
	if err := t.Execute(&buf, hardware); err != nil {
		err = fmt.Errorf("%s: err: %w", fmt.Sprintf(errTemplateParsing, templateID), err)
		return nil, err
	}

	wf, err := parse(buf.Bytes())
	if err != nil {
		return nil, err
	}

	for _, task := range wf.Tasks {
		if task.WorkerAddr == "" {
			return nil, fmt.Errorf("failed to render template, empty hardware address (%v)", hardware)
		}
	}

	return wf, nil
}

// validate validates a workflow template against certain requirements.
func validate(wf *Workflow) error {
	if !hasValidLength(wf.Name) {
		return fmt.Errorf(errInvalidLength, wf.Name)
	}

	if len(wf.Tasks) == 0 {
		return errors.New("template must have at least one task defined")
	}

	taskNameMap := make(map[string]struct{})
	for _, task := range wf.Tasks {
		if !hasValidLength(task.Name) {
			return fmt.Errorf(errInvalidLength, task.Name)
		}

		if _, ok := taskNameMap[task.Name]; ok {
			return fmt.Errorf("two tasks in a template cannot have same name (%s)", task.Name)
		}

		taskNameMap[task.Name] = struct{}{}
		actionNameMap := make(map[string]struct{})
		for _, action := range task.Actions {
			if !hasValidLength(action.Name) {
				return fmt.Errorf(errInvalidLength, action.Name)
			}

			if err := validateImageName(action.Image); err != nil {
				return fmt.Errorf("invalid action image (%s): %v", action.Image, err)
			}

			_, ok := actionNameMap[action.Name]
			if ok {
				return fmt.Errorf("two actions in a task cannot have same name: %s", action.Name)
			}
			actionNameMap[action.Name] = struct{}{}
		}
	}
	return nil
}

func hasValidLength(name string) bool {
	return len(name) > 0 && len(name) < 200
}

func validateImageName(name string) error {
	_, err := reference.ParseNormalizedNamed(name)
	return err
}
