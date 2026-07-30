# frozen_string_literal: true

# The public `brew install <bottle>` command performs mutable tap and prefix
# preflight checks before it reaches the already verified local bottle. The
# materializer deliberately keeps the pinned Homebrew checkout root-owned and
# read-only, so invoke the pinned FormulaInstaller directly instead. Network is
# disabled by the enclosing BuildKit exec, dependencies were resolved by the
# frontend, and the Go materializer independently verifies the bottle before
# this script is called.
require "formulary"
require "formula_installer"

abort "usage: dalec-homebrew-pour.rb <bottle>" unless ARGV.length == 1

bottle_path = Pathname(ARGV.fetch(0)).realpath
abort "bottle path is not a regular file" unless bottle_path.file?

# brew.sh intentionally clears this internal variable at process startup, so
# set it inside the isolated adapter immediately before loading the verified
# local bottle. Unlike HOMEBREW_DEVELOPER, this does not alter installer error
# handling for missing conflict Formulae in the intentionally empty core tap.
ENV["HOMEBREW_INTERNAL_ALLOW_PACKAGES_FROM_PATHS"] = "1"
formula = Formulary.factory(bottle_path, force_bottle: true)
installer = FormulaInstaller.new(
  formula,
  installed_on_request: true,
  force_bottle: true,
  ignore_deps: true,
)
installer.install
installer.finish
exit 1 if Homebrew.failed?
