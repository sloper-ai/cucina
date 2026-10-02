# SPDX-License-Identifier: FSL-1.1-ALv2
#
# tflint for the e2e layers. Only the ruleset bundled with tflint is used so that the check runs
# offline (no plugin download); `tofu validate` and `tofu test` cover the AWS-specific schema.

config {
  call_module_type = "local"
}

plugin "terraform" {
  enabled = true
  preset  = "recommended"
}
