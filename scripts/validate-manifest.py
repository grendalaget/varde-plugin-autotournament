#!/usr/bin/env python3
"""Validate the release manifest with a Varde checkout's real plugin loader."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile

repo = Path(sys.argv[1]).resolve()
manifest = Path(__file__).resolve().parents[1] / 'manifest.example.json'
source = '''package plugins
import ("os"; "testing")
func TestExternalManagedManifest(t *testing.T) {
    registry, err := Load([]string{os.Getenv("EXTERNAL_PLUGIN_MANIFEST")})
    if err != nil { t.Fatal(err) }
    plugin, ok := registry.Get("autotournament")
    if !ok || !plugin.Managed(nil) { t.Fatal("managed default missing") }
    if plugin.Managed(map[string]any{"hosting_mode": "I already have Auto Tournament"}) {
        t.Fatal("manual mode must remain available")
    }
}
'''
with tempfile.TemporaryDirectory(prefix='varde-manifest-') as directory:
    directory = Path(directory)
    test = directory / 'manifest_test.go'
    test.write_text(source)
    overlay = directory / 'overlay.json'
    overlay.write_text(json.dumps({'Replace': {
        str(repo / 'apps/controlplane/internal/plugins/external_manifest_test.go'): str(test)
    }}))
    env = dict(os.environ, EXTERNAL_PLUGIN_MANIFEST=str(manifest),
               VARDE_PLUGIN_SECRET='manifest-validation-only-not-a-real-secret')
    subprocess.run(['go', 'test', '-overlay', str(overlay), './internal/plugins',
                    '-run', '^TestExternalManagedManifest$', '-count=1'],
                   cwd=repo / 'apps/controlplane', env=env, check=True)
    print('Manifest validated by Varde ' + subprocess.check_output(
        ['git', 'rev-parse', 'HEAD'], cwd=repo, text=True).strip())
