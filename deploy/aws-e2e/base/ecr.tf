# SPDX-License-Identifier: FSL-1.1-ALv2

# Temporary ECR repositories for Cucina's own images (§10.1 "Images"). Buildbarn images
# are pulled from ghcr.io by digest; they do not live here.

resource "aws_ecr_repository" "cucina" {
  for_each = toset(var.ecr_repositories)

  name                 = "cucina/${each.value}"
  image_tag_mutability = "MUTABLE"
  # `tofu destroy` must remove the repositories even though they hold images (teardown, T15).
  force_delete = true

  image_scanning_configuration {
    scan_on_push = false
  }

  encryption_configuration {
    encryption_type = "AES256"
  }

  tags = merge(local.tags, { Name = "${local.name}-${each.value}" })
}

# Keep storage cost flat: untagged images expire after a day, tagged ones are capped.
resource "aws_ecr_lifecycle_policy" "cucina" {
  for_each = aws_ecr_repository.cucina

  repository = each.value.name
  policy = jsonencode({
    rules = [
      {
        rulePriority = 1
        description  = "Expire untagged images after 1 day"
        selection = {
          tagStatus   = "untagged"
          countType   = "sinceImagePushed"
          countUnit   = "days"
          countNumber = 1
        }
        action = { type = "expire" }
      },
      {
        rulePriority = 2
        description  = "Keep the 10 most recent images"
        selection = {
          tagStatus   = "any"
          countType   = "imageCountMoreThan"
          countNumber = 10
        }
        action = { type = "expire" }
      },
    ]
  })
}
