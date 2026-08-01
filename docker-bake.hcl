variable "REGISTRY" { default = "ghcr.io/sozercan" }
variable "VERSION" { default = "dev" }
variable "SOURCE_DATE_EPOCH" { default = "1781049600" }
variable "RUNTIME_BASE_AMD64" { default = "docker.io/library/ubuntu@sha256:52df9b1ee71626e0088f7d400d5c6b5f7bb916f8f0c82b474289a4ece6cf3faf" }
variable "RUNTIME_BASE_ARM64" { default = "docker.io/library/ubuntu@sha256:7f622ca8766bccb22f04242ecb6f19f770b2f08827dc4b8c707de5e78a6da7ab" }
variable "RUNTIME_BASE_REF" { default = "" }
variable "MATERIALIZER_REF" { default = "" }
variable "FRONTEND_REF" { default = "" }
variable "HOMEBREW_COMMIT" { default = "77d90328ca2f63ff4ec1f67de0ade5632f5d2335" }
variable "HOMEBREW_RUBY_VERSION" { default = "4.0.6" }
variable "HOMEBREW_KEYS_DIGEST" { default = "sha256:ef2d2c9e0219d485df9f07fff7b037feadc36c93085be9ffefb1390f31a3de1d" }

group "default" { targets = ["frontend"] }
group "release-children" { targets = ["runtime-base-amd64", "runtime-base-arm64", "materializer-amd64", "materializer-arm64"] }

target "runtime-base-amd64" {
  target = "runtime-base"
  platforms = ["linux/amd64"]
  args = { RUNTIME_BASE = RUNTIME_BASE_AMD64, SOURCE_DATE_EPOCH = SOURCE_DATE_EPOCH }
  tags = ["${REGISTRY}/dalec-homebrew-runtime-base:${VERSION}-amd64"]
}

target "runtime-base-arm64" {
  target = "runtime-base"
  platforms = ["linux/arm64"]
  args = { RUNTIME_BASE = RUNTIME_BASE_ARM64, SOURCE_DATE_EPOCH = SOURCE_DATE_EPOCH }
  tags = ["${REGISTRY}/dalec-homebrew-runtime-base:${VERSION}-arm64"]
}

target "materializer-amd64" {
  target = "materializer"
  platforms = ["linux/amd64"]
  args = {
    RUNTIME_BASE = RUNTIME_BASE_AMD64
    SOURCE_DATE_EPOCH = SOURCE_DATE_EPOCH
    HOMEBREW_COMMIT = HOMEBREW_COMMIT
    HOMEBREW_RUBY_VERSION = HOMEBREW_RUBY_VERSION
  }
  tags = ["${REGISTRY}/dalec-homebrew-materializer:${VERSION}-amd64"]
}

target "materializer-arm64" {
  target = "materializer"
  platforms = ["linux/arm64"]
  args = {
    RUNTIME_BASE = RUNTIME_BASE_ARM64
    SOURCE_DATE_EPOCH = SOURCE_DATE_EPOCH
    HOMEBREW_COMMIT = HOMEBREW_COMMIT
    HOMEBREW_RUBY_VERSION = HOMEBREW_RUBY_VERSION
  }
  tags = ["${REGISTRY}/dalec-homebrew-materializer:${VERSION}-arm64"]
}

target "frontend" {
  target = "frontend"
  platforms = ["linux/amd64", "linux/arm64"]
  args = {
    RUNTIME_BASE_REF = RUNTIME_BASE_REF
    MATERIALIZER_REF = MATERIALIZER_REF
    FRONTEND_REF = FRONTEND_REF
    SOURCE_DATE_EPOCH = SOURCE_DATE_EPOCH
    HOMEBREW_COMMIT = HOMEBREW_COMMIT
    HOMEBREW_RUBY_VERSION = HOMEBREW_RUBY_VERSION
    HOMEBREW_KEYS_DIGEST = HOMEBREW_KEYS_DIGEST
  }
  tags = ["${REGISTRY}/dalec-homebrew:${VERSION}"]
}
