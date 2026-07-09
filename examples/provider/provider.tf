terraform {
  required_version = "~> 1.11"
  required_providers {
    ghentapi = {
      source  = "kreemer/ghentapi"
      version = "~> 1.0"
    }
  }
}

provider "ghentapi" {
  base_url = "https://api.github.com"

  enterprise_app_id              = var.ent_app_id
  enterprise_app_installation_id = var.ent_app_installation_id
  enterprise_app_pem_file        = var.ent_pem

  org_app_id        = var.org_app_id
  org_app_client_id = var.org_app_client_id
  org_app_pem_file  = var.org_pem

  # Automatically installs the org app into new organisations on first use.
  auto_install_org_app = true
  repository_selection = "all"
}

variable "ent_app_id" {
  type = string
}

variable "ent_app_installation_id" {
  type = string
}

variable "ent_pem" {
  type      = string
  sensitive = true
}

variable "org_app_id" {
  type = string
}

variable "org_app_client_id" {
  type = string
}

variable "org_pem" {
  type      = string
  sensitive = true
}
