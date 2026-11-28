cask "agentrm" do
  version "0.3.0"
  sha256 "870fec6ba98d3387b4c6524c49dbd501db7b1019aac619b2311a741b9758f841"

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
