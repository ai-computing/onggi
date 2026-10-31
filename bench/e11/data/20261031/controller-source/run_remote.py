import argparse
from pathlib import Path
import sys
from unittest.mock import patch

sys.path.insert(0, 'C:/Users/USER/orca/workspaces/Plane/pollock')
from hub import orca

p = argparse.ArgumentParser()
p.add_argument('environment')
p.add_argument('worktree')
p.add_argument('script', type=Path)
p.add_argument('--timeout', type=float, default=120)
a = p.parse_args()
original = orca.call

def scoped(*args, timeout=60):
    return original(*args, '--environment', a.environment, timeout=timeout)

with patch.object(orca, 'call', scoped):
    rc, output = orca.run_remote(a.worktree, a.script.read_text(encoding='utf-8'), timeout=a.timeout)
print(output, end='')
sys.exit(rc)
