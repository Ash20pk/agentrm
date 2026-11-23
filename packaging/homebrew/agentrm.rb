cask "agentrm" do
  version "0.2.0"
  sha256 "8f52012447692908ff86f179bdd5d9e4276090033fa156c2ffd9e16e57bc5ced"

  url "https://github.com/Ash20pk/agentrm/releases/download/v#{version}/AgentRM-#{version}.dmg"
  name "AgentRM"
  desc "Resource manager for coding agents: leases for heavy work, reversible stop for what nobody claims"
  homepage "https://github.com/Ash20pk/agentrm"

  livecheck do
    url :url
    strategy :github_latest
  end

  auto_updates true
  depends_on macos: :sonoma

  app "AgentRM.app"
  binary "#{appdir}/AgentRM.app/Contents/Helpers/agentrm"

  uninstall_preflight do
    # Remove the hooks, the shim and the Codex note, and resume anything paused.
    system_command "#{appdir}/AgentRM.app/Contents/Helpers/agentrm",
                   args:         ["setup", "uninstall"],
                   must_succeed: false
  end

  uninstall launchctl: "dev.agentrm.daemon",
            quit:      "dev.agentrm.AgentRM"

  zap trash: [
    "~/.agentrm",
    "~/Library/Preferences/dev.agentrm.AgentRM.plist",
  ]
end
