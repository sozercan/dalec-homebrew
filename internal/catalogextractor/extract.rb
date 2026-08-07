# frozen_string_literal: true

require "digest"
require "find"
require "json"
require "pathname"
require "formulary"
require "simulate_system"
require "tap"
require "uri"
require "utils/bottles"

MAX_FORMULAE = 4096
MAX_FORMULA_BYTES = 4 * 1024 * 1024
PLATFORM_TAGS = %w[x86_64_linux arm64_linux].freeze
SUPPORTED_TAGS = (PLATFORM_TAGS + %w[all]).freeze

def abort_with(message)
  warn "dalec-homebrew catalog extractor: #{message}"
  exit 1
end

def bottle_from_hash(hash)
  stable = hash.fetch("bottle", {}).fetch("stable", nil)
  return nil if stable.nil? || stable.empty?

  files = stable.fetch("files", {}).filter_map do |tag, value|
    tag_name = tag.to_s
    next unless SUPPORTED_TAGS.include?(tag_name)

    cellar = value.fetch("cellar").to_s
    cellar = ENV.fetch("HOMEBREW_CELLAR") if cellar == "/Cellar"
    {
      "tag" => tag_name,
      "url" => value.fetch("url"),
      "sha256" => "sha256:#{value.fetch('sha256')}",
      "cellar" => cellar,
    }
  end.sort_by { |file| file.fetch("tag") }
  {
    "root_url" => stable.fetch("root_url"),
    "rebuild" => stable.fetch("rebuild", 0),
    "files" => files,
  }
end

def stable_archive_format(url)
  uri = URI.parse(url)
  return nil unless uri.is_a?(URI::HTTPS) && uri.userinfo.nil? && uri.fragment.nil? && uri.port == 443

  return "tar+gzip" if uri.path.end_with?(".tar.gz", ".tgz")

  nil
rescue URI::InvalidURIError
  nil
end

def stable_source_from_hash(hash)
  stable = hash.fetch("urls", {}).fetch("stable", nil)
  return nil unless stable.is_a?(Hash)
  return nil unless stable.fetch("tag", nil).nil? && stable.fetch("revision", nil).nil? && stable.fetch("using", nil).nil?

  url = stable.fetch("url", "").to_s
  checksum = stable.fetch("checksum", "").to_s
  format = stable_archive_format(url)
  return nil if format.nil? || !checksum.match?(/\A[0-9a-f]{64}\z/)

  {
    "url" => url,
    "sha256" => "sha256:#{checksum}",
    "archive_format" => format,
  }
end

def platform_formula(path, tag_name)
  tag = Utils::Bottles::Tag.from_symbol(tag_name.to_sym)
  simulated_arch = { x86_64: :intel, arm64: :arm }.fetch(tag.arch)
  Homebrew::SimulateSystem.with(os: tag.system, arch: simulated_arch) do
    Formulary.clear_cache
    formula = Formulary.factory(path, :stable, force_bottle: true)
    hash = formula.to_hash
    stable = hash.fetch("versions", {}).fetch("stable", nil)
    return nil if stable.nil? || stable.empty?

    uses_from_macos = hash.fetch("uses_from_macos", []).flat_map do |entry|
      entry.is_a?(Hash) ? entry.keys : [entry]
    end
    {
      "tag" => tag_name,
      "name" => formula.name,
      "homebrew_full_name" => formula.full_name,
      "stable_version" => stable,
      "revision" => hash.fetch("revision", 0),
      "version_scheme" => hash.fetch("version_scheme", 0),
      "disabled" => hash.fetch("disabled", false),
      "keg_only" => hash.fetch("keg_only", false),
      "license" => hash.fetch("license", "").to_s,
      "dependencies" => (hash.fetch("dependencies", []) + hash.fetch("recommended_dependencies", []) + uses_from_macos).uniq,
      "versioned_formulae" => hash.fetch("versioned_formulae", []),
      "bottle" => bottle_from_hash(hash),
      "stable_source" => stable_source_from_hash(hash),
    }.compact
  end
end

unless ARGV.length == 4
  abort_with("usage: extract.rb OWNER/TAP REPOSITORY TAP_ROOT SOURCE_METADATA")
end

tap_id, repository, tap_root, source_metadata_path = ARGV
owner, tap_name, extra = tap_id.split("/", 3)
abort_with("tap identity must be owner/tap") if owner.nil? || tap_name.nil? || !extra.nil?
expected_root = Pathname(ENV.fetch("HOMEBREW_REPOSITORY")) / "Library" / "Taps" / owner / "homebrew-#{tap_name}"
tap_path = Pathname(tap_root).realpath
abort_with("tap root is not mounted at its canonical Homebrew path") unless tap_path == expected_root.realpath

source_metadata = JSON.parse(Pathname(source_metadata_path).read)
source_tap = source_metadata.fetch("tap")
abort_with("source metadata changed tap identity") unless source_tap.fetch("id") == tap_id && source_tap.fetch("repository") == repository

tap = Tap.fetch(owner, tap_name)
abort_with("tap is unavailable") unless tap.installed?
formula_paths = tap.formula_files.sort_by(&:to_s)
abort_with("tap contains no Formulae") if formula_paths.empty?
abort_with("tap contains more than #{MAX_FORMULAE} Formulae") if formula_paths.length > MAX_FORMULAE

tap_prefix = "#{tap_path}/"
Find.find(tap_path.to_s) do |entry|
  candidate = Pathname(entry)
  next unless candidate.symlink?

  resolved = candidate.realpath
  abort_with("tap symlink #{candidate.relative_path_from(tap_path)} escapes the authenticated tree") unless resolved.to_s.start_with?(tap_prefix)
rescue Errno::ENOENT, Errno::ELOOP
  abort_with("tap symlink #{candidate.relative_path_from(tap_path)} is dangling or cyclic")
end

prepared_formulae = formula_paths.map do |formula_path|
  relative = formula_path.relative_path_from(tap_path).to_s
  stat = formula_path.lstat
  abort_with("Formula #{relative} is not a non-symlink regular file") if stat.symlink? || !stat.file?
  resolved = formula_path.realpath
  abort_with("Formula #{relative} escapes the authenticated tap") unless resolved.to_s.start_with?(tap_prefix)
  abort_with("Formula #{relative} exceeds #{MAX_FORMULA_BYTES} bytes") if stat.size > MAX_FORMULA_BYTES
  source = formula_path.binread
  [formula_path, relative, "sha256:#{Digest::SHA256.hexdigest(source)}"]
end

formulae = prepared_formulae.filter_map do |formula_path, relative, source_digest|
  platforms = PLATFORM_TAGS.filter_map { |tag| platform_formula(formula_path, tag) }
  next if platforms.empty?

  {
    "source_path" => relative,
    "source_digest" => source_digest,
    "platforms" => platforms,
  }
rescue => e
  abort_with("Formula #{relative}: #{e.class}: #{e.message}")
end
abort_with("tap contains no current stable Formulae") if formulae.empty?

formula_names = formulae.map { |entry| entry.fetch("platforms").first.fetch("name") }.to_h { |name| [name, true] }
short_name = ->(value) { Utils.name_from_full_name(value) }
aliases = tap.alias_table.each_with_object({}) do |(from, to), result|
  target = short_name.call(to)
  result[short_name.call(from)] = target if formula_names[target]
end.sort.to_h
renames = tap.formula_renames.each_with_object({}) do |(from, to), result|
  target = short_name.call(to)
  result[short_name.call(from)] = target if formula_names[target]
end.sort.to_h
migrations = tap.tap_migrations.sort.to_h

result = {
  "schema_version" => "dalec-homebrew-extracted-tap/v1",
  "tap" => source_tap,
  "formulae" => formulae,
  "aliases" => aliases,
  "renames" => renames,
  "migrations" => migrations,
}
STDOUT.write(JSON.generate(result))
