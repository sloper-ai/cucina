# SPDX-License-Identifier: FSL-1.1-ALv2

# IAM for the acceptance environment (PROMPT.md §10.1, R-POOL-3, R-CP-8).
#
#   cucina-e2e-controller (policy)  least-privilege fleet control: launch only with the three
#                                   cucina:* request tags, destroy only what is tagged
#                                   cucina:env=e2e, never anything tagged cucina:protected=true
#                                   (the k3s node and the clients), pass only the worker role.
#   cucina-e2e-worker     (role)    SSM core + read of /cucina/e2e/workers/*; NO EC2 permissions.
#   cucina-e2e-k3s-node   (role)    SSM core + ECR pull + the controller policy (k3s uses the
#                                   node's instance profile via IMDSv2 hop-limit 2; acceptable
#                                   only for this temporary cluster, R-CP-8).
#   cucina-e2e-client     (role)    SSM core + read of /cucina/e2e/clients/*.
#
# Policy documents are plain HCL objects run through jsonencode(), so the offline `tofu test`
# suite can decode and assert on them (data.aws_iam_policy_document is evaluated by the
# provider and cannot be inspected under mock_provider).

locals {
  ec2_arn   = "arn:${local.partition}:ec2:${var.region}:${local.account_id}" # instance, volume, ENI, subnet, SG, fleet, launch-template
  ec2_noacc = "arn:${local.partition}:ec2:${var.region}"                     # image and snapshot ARNs carry no account id
  ssm_param = "arn:${local.partition}:ssm:${var.region}:${local.account_id}:parameter"
  ecr_repos = "arn:${local.partition}:ecr:${var.region}:${local.account_id}:repository/cucina/*"

  # Condition fragments reused below.
  cond_env_resource = { StringEquals = { "aws:ResourceTag/cucina:env" = "e2e" } }
  cond_env_request = {
    StringEquals = { "aws:RequestTag/cucina:env" = "e2e" }
    # All three tags must be present on every created resource (§12), not just cucina:env.
    Null = {
      "aws:RequestTag/cucina:run"     = "false"
      "aws:RequestTag/cucina:expires" = "false"
    }
  }
  protective_tag_keys = ["cucina:env", "cucina:run", "cucina:expires", "cucina:protected"]

  ecr_pull_statements = [
    {
      Sid      = "EcrAuthToken"
      Effect   = "Allow"
      Action   = ["ecr:GetAuthorizationToken"]
      Resource = "*"
    },
    {
      Sid    = "EcrPullCucinaRepositories"
      Effect = "Allow"
      Action = [
        "ecr:BatchCheckLayerAvailability",
        "ecr:BatchGetImage",
        "ecr:GetDownloadUrlForLayer",
        "ecr:DescribeImages",
      ]
      Resource = local.ecr_repos
    },
  ]

  controller_policy = {
    Version = "2012-10-17"
    Statement = concat([
      {
        Sid      = "ReadOnlyDescribe"
        Effect   = "Allow"
        Action   = ["ec2:Describe*"]
        Resource = "*"
      },
      {
        Sid      = "PriceList"
        Effect   = "Allow"
        Action   = ["pricing:GetProducts", "pricing:DescribeServices", "pricing:GetAttributeValues"]
        Resource = "*"
      },
      {
        # Instances, their volumes and ENIs can only be created when the request carries all
        # three cucina:* tags (cucina:env = e2e), i.e. TagSpecifications on every resource type.
        Sid    = "LaunchRequiresTags"
        Effect = "Allow"
        Action = ["ec2:RunInstances", "ec2:CreateFleet"]
        Resource = [
          "${local.ec2_arn}:instance/*",
          "${local.ec2_arn}:volume/*",
          "${local.ec2_arn}:network-interface/*",
          "${local.ec2_arn}:spot-instances-request/*",
        ]
        Condition = local.cond_env_request
      },
      {
        # Everything an instance is launched FROM or INTO must itself belong to the e2e env:
        # images, snapshots, the subnet, security groups and (launch / Fast Launch prep) templates.
        Sid    = "LaunchUsesTaggedResourcesOnly"
        Effect = "Allow"
        Action = ["ec2:RunInstances", "ec2:CreateFleet"]
        Resource = [
          "${local.ec2_noacc}::image/*",
          "${local.ec2_noacc}::snapshot/*",
          "${local.ec2_arn}:subnet/*",
          "${local.ec2_arn}:security-group/*",
          "${local.ec2_arn}:launch-template/*",
        ]
        Condition = local.cond_env_resource
      },
      {
        Sid       = "CreateFleetRequiresTags"
        Effect    = "Allow"
        Action    = ["ec2:CreateFleet"]
        Resource  = ["${local.ec2_arn}:fleet/*"]
        Condition = local.cond_env_request
      },
      {
        Sid       = "LaunchTemplateCreateRequiresTags"
        Effect    = "Allow"
        Action    = ["ec2:CreateLaunchTemplate"]
        Resource  = ["${local.ec2_arn}:launch-template/*"]
        Condition = local.cond_env_request
      },
      {
        Sid    = "LaunchTemplateManageTagged"
        Effect = "Allow"
        Action = [
          "ec2:CreateLaunchTemplateVersion",
          "ec2:ModifyLaunchTemplate",
          "ec2:DeleteLaunchTemplate",
          "ec2:DeleteLaunchTemplateVersions",
        ]
        Resource  = ["${local.ec2_arn}:launch-template/*"]
        Condition = local.cond_env_resource
      },
      {
        # Tagging while a resource is being created (TagSpecifications).
        Sid    = "TagOnCreate"
        Effect = "Allow"
        Action = ["ec2:CreateTags"]
        Resource = [
          "${local.ec2_arn}:instance/*",
          "${local.ec2_arn}:volume/*",
          "${local.ec2_arn}:network-interface/*",
          "${local.ec2_arn}:spot-instances-request/*",
          "${local.ec2_arn}:fleet/*",
          "${local.ec2_arn}:launch-template/*",
        ]
        Condition = {
          StringEquals = {
            "ec2:CreateAction"          = ["RunInstances", "CreateFleet", "CreateLaunchTemplate"]
            "aws:RequestTag/cucina:env" = "e2e"
          }
        }
      },
      {
        # Pool/generation/state tags on existing e2e resources; the protective tags are immutable.
        Sid    = "TagExistingTagged"
        Effect = "Allow"
        Action = ["ec2:CreateTags", "ec2:DeleteTags"]
        Resource = [
          "${local.ec2_arn}:instance/*",
          "${local.ec2_arn}:volume/*",
          "${local.ec2_arn}:network-interface/*",
          "${local.ec2_arn}:launch-template/*",
        ]
        Condition = {
          StringEquals                   = { "aws:ResourceTag/cucina:env" = "e2e" }
          "ForAllValues:StringNotEquals" = { "aws:TagKeys" = local.protective_tag_keys }
        }
      },
      {
        Sid       = "TerminateTaggedInstances"
        Effect    = "Allow"
        Action    = ["ec2:TerminateInstances"]
        Resource  = ["${local.ec2_arn}:instance/*"]
        Condition = local.cond_env_resource
      },
      {
        Sid       = "DeleteTaggedVolumes"
        Effect    = "Allow"
        Action    = ["ec2:DeleteVolume"]
        Resource  = ["${local.ec2_arn}:volume/*"]
        Condition = local.cond_env_resource
      },
      {
        Sid       = "DeleteTaggedNetworkInterfaces"
        Effect    = "Allow"
        Action    = ["ec2:DeleteNetworkInterface"]
        Resource  = ["${local.ec2_arn}:network-interface/*"]
        Condition = local.cond_env_resource
      },
      {
        Sid      = "PassOnlyTheWorkerRole"
        Effect   = "Allow"
        Action   = ["iam:PassRole"]
        Resource = [aws_iam_role.worker.arn]
        Condition = {
          StringEquals = { "iam:PassedToService" = "ec2.amazonaws.com" }
        }
      },
      {
        Sid      = "ReadWorkerInstanceProfile"
        Effect   = "Allow"
        Action   = ["iam:GetInstanceProfile"]
        Resource = [aws_iam_instance_profile.worker.arn]
      },
      {
        Sid      = "ReadEnvParameters"
        Effect   = "Allow"
        Action   = ["ssm:GetParameter", "ssm:GetParameters", "ssm:GetParametersByPath"]
        Resource = ["${local.ssm_param}/cucina/e2e/*"]
      },
      {
        # Never touch the infrastructure nodes (k3s node, clients), whatever else is allowed.
        Sid    = "DenyProtectedInfrastructure"
        Effect = "Deny"
        Action = [
          "ec2:TerminateInstances",
          "ec2:StopInstances",
          "ec2:RebootInstances",
          "ec2:ModifyInstanceAttribute",
          "ec2:DeleteVolume",
          "ec2:DetachVolume",
          "ec2:DeleteNetworkInterface",
          "ec2:DeleteTags",
          "ec2:CreateTags",
        ]
        Resource  = "*"
        Condition = { StringEquals = { "aws:ResourceTag/cucina:protected" = "true" } }
      },
      {
        Sid      = "DenyOutsideUsWest1"
        Effect   = "Deny"
        Action   = ["ec2:*", "ssm:*", "ecr:*"]
        Resource = "*"
        Condition = {
          StringNotEquals = { "aws:RequestedRegion" = var.region }
        }
      },
    ], local.ecr_pull_statements)
  }

  # Image lifecycle, kept in its own managed policy (the controller policy is close to IAM's size limit).
  # EnableFastLaunch is authorized against the prep launch template as well as the image, and runs a
  # RunInstances DRY RUN as the caller without request tags; that is allowed only when it launches from
  # the (tagged) prep template, whose tag specifications tag everything it launches.
  controller_images_policy = {
    Version = "2012-10-17"
    Statement = [
      {
        # Image lifecycle incl. EC2 Fast Launch (enable on new AMIs, disable before deregistering).
        Sid       = "ImagesAndFastLaunchTagged"
        Effect    = "Allow"
        Action    = ["ec2:DeregisterImage", "ec2:EnableFastLaunch", "ec2:DisableFastLaunch"]
        Resource  = ["${local.ec2_noacc}::image/*"]
        Condition = local.cond_env_resource
      },
      {
        # EnableFastLaunch is authorized against the prep launch template as well as the image.
        Sid       = "FastLaunchUsesPrepTemplate"
        Effect    = "Allow"
        Action    = ["ec2:EnableFastLaunch", "ec2:DisableFastLaunch"]
        Resource  = ["${local.ec2_arn}:launch-template/*"]
        Condition = local.cond_env_resource
      },
      {
        Sid       = "DeleteTaggedSnapshots"
        Effect    = "Allow"
        Action    = ["ec2:DeleteSnapshot"]
        Resource  = ["${local.ec2_noacc}::snapshot/*"]
        Condition = local.cond_env_resource
      },
      {
        Sid      = "FastLaunchPrepDryRunLaunch"
        Effect   = "Allow"
        Action   = ["ec2:RunInstances"]
        Resource = ["${local.ec2_arn}:instance/*", "${local.ec2_arn}:volume/*", "${local.ec2_arn}:network-interface/*"]
        Condition = {
          ArnEquals = { "ec2:LaunchTemplate" = aws_launch_template.fast_launch_prep.arn }
        }
      },
    ]
  }

  # The AWS-managed AmazonSSMManagedInstanceCore grants ssm:GetParameter(s) on "*". Every role
  # below therefore carries an explicit deny on parameter reads outside its own prefix.
  worker_policy = {
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "ReadOwnPoolParameters"
        Effect   = "Allow"
        Action   = ["ssm:GetParameter", "ssm:GetParameters", "ssm:GetParametersByPath"]
        Resource = ["${local.ssm_param}/cucina/e2e/workers/*"]
      },
      {
        Sid         = "DenyParameterReadsElsewhere"
        Effect      = "Deny"
        Action      = ["ssm:GetParameter", "ssm:GetParameters", "ssm:GetParametersByPath"]
        NotResource = ["${local.ssm_param}/cucina/e2e/workers/*"]
      },
    ]
  }

  client_policy = {
    Version = "2012-10-17"
    Statement = [
      {
        Sid      = "ReadClientParameters"
        Effect   = "Allow"
        Action   = ["ssm:GetParameter", "ssm:GetParameters", "ssm:GetParametersByPath"]
        Resource = ["${local.ssm_param}/cucina/e2e/clients/*"]
      },
      {
        Sid         = "DenyParameterReadsElsewhere"
        Effect      = "Deny"
        Action      = ["ssm:GetParameter", "ssm:GetParameters", "ssm:GetParametersByPath"]
        NotResource = ["${local.ssm_param}/cucina/e2e/clients/*"]
      },
    ]
  }

  k3s_node_policy = {
    Version = "2012-10-17"
    Statement = [
      {
        Sid         = "DenyParameterReadsOutsideEnvPrefix"
        Effect      = "Deny"
        Action      = ["ssm:GetParameter", "ssm:GetParameters", "ssm:GetParametersByPath"]
        NotResource = ["${local.ssm_param}/cucina/e2e/*"]
      },
    ]
  }

  ecr_pull_policy = {
    Version   = "2012-10-17"
    Statement = local.ecr_pull_statements
  }

  ec2_trust_policy = {
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  }

  ssm_core_policy_arn = "arn:${local.partition}:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

# --- policies ---------------------------------------------------------------------------

resource "aws_iam_policy" "controller" {
  name        = "${local.name}-controller"
  description = "Cucina controller: tag-gated EC2 fleet control, Fast Launch, price list, env SSM parameters"
  policy      = jsonencode(local.controller_policy)

  tags = merge(local.tags, { Name = "${local.name}-controller" })
}

resource "aws_iam_policy" "controller_images" {
  name        = "${local.name}-controller-images"
  description = "Cucina controller: deregister tagged images, Fast Launch enable/disable via the prep launch template"
  policy      = jsonencode(local.controller_images_policy)

  tags = merge(local.tags, { Name = "${local.name}-controller-images" })
}

resource "aws_iam_policy" "ecr_pull" {
  name        = "${local.name}-ecr-pull"
  description = "Pull Cucina images from the cucina/* ECR repositories"
  policy      = jsonencode(local.ecr_pull_policy)

  tags = merge(local.tags, { Name = "${local.name}-ecr-pull" })
}

# --- worker role (R-POOL-3: no EC2 permissions) -------------------------------------------

resource "aws_iam_role" "worker" {
  name               = "${local.name}-worker"
  description        = "Cucina EC2 workers: SSM core and own-pool parameters only"
  assume_role_policy = jsonencode(local.ec2_trust_policy)

  tags = merge(local.tags, { Name = "${local.name}-worker" })
}

resource "aws_iam_role_policy_attachment" "worker_ssm_core" {
  role       = aws_iam_role.worker.name
  policy_arn = local.ssm_core_policy_arn
}

resource "aws_iam_role_policy" "worker" {
  name   = "own-pool-parameters"
  role   = aws_iam_role.worker.id
  policy = jsonencode(local.worker_policy)
}

resource "aws_iam_instance_profile" "worker" {
  name = "${local.name}-worker"
  role = aws_iam_role.worker.name

  tags = merge(local.tags, { Name = "${local.name}-worker" })
}

# --- k3s node role (R-CP-8: IMDSv2 hop-limit 2, temporary cluster only) --------------------

resource "aws_iam_role" "k3s_node" {
  name               = "${local.name}-k3s-node"
  description        = "k3s node: SSM core, ECR pull and the controller policy (temporary cluster)"
  assume_role_policy = jsonencode(local.ec2_trust_policy)

  tags = merge(local.tags, { Name = "${local.name}-k3s-node" })
}

resource "aws_iam_role_policy_attachment" "k3s_node_ssm_core" {
  role       = aws_iam_role.k3s_node.name
  policy_arn = local.ssm_core_policy_arn
}

resource "aws_iam_role_policy_attachment" "k3s_node_controller" {
  role       = aws_iam_role.k3s_node.name
  policy_arn = aws_iam_policy.controller.arn
}

resource "aws_iam_role_policy_attachment" "k3s_node_controller_images" {
  role       = aws_iam_role.k3s_node.name
  policy_arn = aws_iam_policy.controller_images.arn
}

resource "aws_iam_role_policy_attachment" "k3s_node_ecr_pull" {
  role       = aws_iam_role.k3s_node.name
  policy_arn = aws_iam_policy.ecr_pull.arn
}

resource "aws_iam_role_policy" "k3s_node" {
  name   = "parameter-read-boundary"
  role   = aws_iam_role.k3s_node.id
  policy = jsonencode(local.k3s_node_policy)
}

resource "aws_iam_instance_profile" "k3s_node" {
  name = "${local.name}-k3s-node"
  role = aws_iam_role.k3s_node.name

  tags = merge(local.tags, { Name = "${local.name}-k3s-node" })
}

# --- client role -------------------------------------------------------------------------

resource "aws_iam_role" "client" {
  name               = "${local.name}-client"
  description        = "Bazel client VMs and Packer builders: SSM core only"
  assume_role_policy = jsonencode(local.ec2_trust_policy)

  tags = merge(local.tags, { Name = "${local.name}-client" })
}

resource "aws_iam_role_policy_attachment" "client_ssm_core" {
  role       = aws_iam_role.client.name
  policy_arn = local.ssm_core_policy_arn
}

resource "aws_iam_role_policy" "client" {
  name   = "client-parameters"
  role   = aws_iam_role.client.id
  policy = jsonencode(local.client_policy)
}

resource "aws_iam_instance_profile" "client" {
  name = "${local.name}-client"
  role = aws_iam_role.client.name

  tags = merge(local.tags, { Name = "${local.name}-client" })
}
