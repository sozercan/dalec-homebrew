# frozen_string_literal: true

# The public `brew install <bottle>` command performs mutable tap and prefix
# preflight checks before it reaches the already verified local bottle. The
# materializer deliberately keeps the pinned Homebrew checkout root-owned and
# read-only, so invoke the pinned FormulaInstaller directly instead. Network is
# disabled by the enclosing BuildKit exec, dependencies were resolved by the
# frontend, and the Go materializer independently verifies the bottle before
# this script is called.
require "digest"
require "formulary"
require "formula_installer"

unless [1, 3, 4].include?(ARGV.length)
  abort "usage: dalec-homebrew-pour.rb <bottle> OR <formula-id> <staged-formula> <bottle> [derived-prebuilt-v1]"
end

derived_prebuilt = ARGV.length == 4
abort "unsupported derived-bottle mode" if derived_prebuilt && ARGV.fetch(3) != "derived-prebuilt-v1"

if ARGV.length == 1
  bottle_path = Pathname(ARGV.fetch(0)).realpath
  abort "bottle path is not a regular file" unless bottle_path.file?

  # V1 compatibility: the legacy adapter loads the verified local bottle path
  # directly. V2 never enables this broad path-loader switch.
  ENV["HOMEBREW_INTERNAL_ALLOW_PACKAGES_FROM_PATHS"] = "1"
  formula = Formulary.factory(bottle_path, force_bottle: true)
else
  formula_id = ARGV.fetch(0)
  formula_path = Pathname(ARGV.fetch(1)).realpath
  bottle_path = Pathname(ARGV.fetch(2)).realpath
  abort "staged Formula path is not a regular file" unless formula_path.file?
  abort "bottle path is not a regular file" unless bottle_path.file?

  # The staged file contains the independently verified bottle-embedded
  # Formula source, but lives under its explicit synthetic Tap path. Loading
  # this path preserves Homebrew's tap-trust check and exact Tap identity.
  formula = Formulary.factory(formula_path, force_bottle: true)
  actual_id = if formula.tap&.core_tap?
    "homebrew/core/#{formula.name}"
  else
    formula.full_name
  end
  abort "staged Formula identity mismatch" unless actual_id == formula_id

  # FormulaInstaller treats this exact local path as the only bottle source.
  # Network and dependency/source fallback remain disabled by the enclosing
  # materializer execution and installer options.
  formula.local_bottle_path = bottle_path

  if derived_prebuilt
    # The authenticated Formula intentionally has no upstream bottle block.
    # Attach only the local derived bottle's relocation policy so Homebrew can
    # pour it without changing or evaluating the Formula install method.
    checksum = Digest::SHA256.file(bottle_path).hexdigest
    formula.bottle_specification.sha256(
      cellar: :any_skip_relocation,
      Utils::Bottles.tag.to_sym => checksum,
    )
  end
end

installer = FormulaInstaller.new(
  formula,
  installed_on_request: true,
  force_bottle: true,
  ignore_deps: true,
  skip_post_install: derived_prebuilt,
)
installer.install

# FormulaInstaller#finish updates Homebrew's optional linkage cache. Actual
# dynamic-library inspection is complete before check_formula_deps, but the
# latter also classifies declared dependencies by loading Formula objects. The
# core tap is intentionally empty in the offline materializer, so retain the
# real linkage scan and skip only that metadata classification when a Formula
# is unavailable. The Go materializer still verifies the complete installed
# shared-library closure and propagates every other Homebrew failure.
module DalecHomebrewOfflineLinkage
  private

  def check_formula_deps
    super
  rescue FormulaUnavailableError
    nil
  end
end
LinkageChecker.prepend(DalecHomebrewOfflineLinkage)

installer.finish
exit 1 if Homebrew.failed?
