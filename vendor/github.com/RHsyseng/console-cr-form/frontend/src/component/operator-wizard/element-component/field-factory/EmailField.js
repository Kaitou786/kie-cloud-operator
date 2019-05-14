import React from "react";
import validator from "validator";
import { FormGroup, TextInput, Tooltip } from "@patternfly/react-core";

export class EmailField {
  constructor(props) {
    this.props = props;
    this.onBlurText = this.onBlurText.bind(this);
    this.value = "";
    this.errMsg = "";
    this.isValid = true;
  }

  getJsx() {
    this.value = this.props.fieldDef.value;
    this.isValidField(this.value);

    return (
      <FormGroup
        label={this.props.fieldDef.label}
        fieldId={this.props.ids.fieldGroupId}
        key={this.props.ids.fieldGroupKey}
        helperTextInvalid={this.errMsg}
        isValid={this.isValid}
        isRequired={this.props.fieldDef.required}
      >
        <Tooltip
          position="left"
          content={<div>{this.props.fieldDef.description}</div>}
          enableFlip={true}
          style={{
            display:
              this.props.fieldDef.description !== undefined &&
              this.props.fieldDef.description !== ""
                ? "block"
                : "none"
          }}
        >
          <TextInput
            type="text"
            id={this.props.ids.fieldId}
            key={this.props.ids.fieldKey}
            aria-describedby="horizontal-form-name-helper"
            name={this.props.fieldDef.label}
            onBlur={this.onBlurText}
            jsonpath={this.props.fieldDef.jsonPath}
            defaultValue={this.value}
            {...this.props.attrs}
          />
        </Tooltip>
      </FormGroup>
    );
  }

  onBlurText = event => {
    //debugger;
    let value = event.target.value;
    if (value !== undefined && value !== null) {
      this.isValidField(value);
      this.props.fieldDef.value = value;
      this.value = value;
      //this.props.fieldDef.default = value;
    }
  };

  isValidField(value) {
    if (
      this.props.fieldDef.required === true &&
      (value === undefined || value === "")
    ) {
      this.errMsg = this.props.fieldDef.label + " is required.";
      this.isValid = false;
    } else if (value !== "" && !validator.isEmail(value)) {
      this.errMsg = value + " is not a valid email.";
      this.isValid = false;
    } else {
      this.errMsg = "";
      this.isValid = true;
    }
  }
}
