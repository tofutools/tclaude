# Config management (Ansible)

If you manage servers with Ansible (or Salt, Chef, Nix), the systemd user unit
from [systemd](systemd.md) is just a templated file plus lingering. The play
below is a generic illustration. Adapt the variables, and add the
[tmux runtime unit](systemd.md#keeping-agents-alive-across-agentd-restarts)
the same way if you want cgroup delegation.

It assumes `tclaude`, `tmux`, and the harness CLIs are already installed for
`tclaude_user`, and that the harness logins are done (they are interactive,
see [Running tclaude on a server](index.md#what-a-headless-box-changes)).

`tclaude-agentd.service.j2`:

```ini
[Unit]
Description=tclaude agentd

[Service]
Type=simple
Environment=PATH={{ tclaude_path }}
ExecStart={{ tclaude_bin }} agentd serve --no-tray --dashboard-port {{ tclaude_dashboard_port }} --persist-operator-token
KillMode=process
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

`tclaude.yml`:

```yaml
- hosts: tclaude_servers
  become: true
  vars:
    tclaude_user: tclaude
    tclaude_home: "/home/{{ tclaude_user }}"
    tclaude_bin: "{{ tclaude_home }}/.local/bin/tclaude"
    tclaude_dashboard_port: 7878
    tclaude_path: "{{ tclaude_home }}/.local/bin:{{ tclaude_home }}/.local/share/mise/shims:/usr/local/bin:/usr/bin:/bin"
  tasks:
    - name: Keep the user manager running without a login
      ansible.builtin.command: loginctl enable-linger {{ tclaude_user }}
      args:
        creates: "/var/lib/systemd/linger/{{ tclaude_user }}"

    - name: Create the user unit directory
      ansible.builtin.file:
        path: "{{ tclaude_home }}/.config/systemd/user"
        state: directory
        owner: "{{ tclaude_user }}"
        mode: "0755"

    - name: Look up the user's UID
      ansible.builtin.command: id -u {{ tclaude_user }}
      register: uid
      changed_when: false

    - name: Install the agentd user unit
      ansible.builtin.template:
        src: tclaude-agentd.service.j2
        dest: "{{ tclaude_home }}/.config/systemd/user/tclaude-agentd.service"
        owner: "{{ tclaude_user }}"
        mode: "0644"
      register: unit

    - name: Enable and (re)start agentd
      become_user: "{{ tclaude_user }}"
      environment:
        XDG_RUNTIME_DIR: "/run/user/{{ uid.stdout }}"
      ansible.builtin.systemd_service:
        name: tclaude-agentd.service
        scope: user
        enabled: true
        state: "{{ 'restarted' if unit.changed else 'started' }}"
        daemon_reload: "{{ unit.changed }}"
```

Notes:

- Because the unit uses `KillMode=process`, a `restarted` from the play
  restarts only `agentd`. Running agents stay up. See
  [Restarts](systemd.md#restarts-and-whether-the-agents-survive-them).
- `systemctl --user` needs the target user's `XDG_RUNTIME_DIR`, hence the
  `environment:` block. Lingering creates `/run/user/<uid>`.
- Template the `PATH` the harnesses actually need on your hosts. After the
  first run, `tclaude harness ls` as that user shows what the daemon finds.
- Don't template `--print-human-token=always`, or any operator token, into the
  unit. The token stays in the account's 0600
  `~/.tclaude/data/operator_token`.

## Then open the dashboard

Tunnel to the configured port and open a sign-in link as the service user:

```bash
ssh -L 7878:127.0.0.1:7878 you@server
sudo -iu tclaude tclaude agent dashboard --print
```

See [Dashboard](../dashboard.md).
