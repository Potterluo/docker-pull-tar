"use client";

import { useCallback, useEffect, useState } from "react";
import { KeyRound, Pencil, Plus, Trash2 } from "lucide-react";
import {
  createCredential,
  deleteCredential,
  listCredentials,
  listRegistries,
  updateCredential,
  type BuiltinRegistry,
  type Credential,
} from "@/lib/api";
import { Button } from "@/components/ui/button";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";

/**
 * /credentials — stored registry logins for private images.
 *
 * The page never receives a stored password: the API is write-only for secrets
 * (`hasSecret` is all the client learns). Editing a row therefore leaves the
 * password field blank, and blank means "keep the existing one" — which is why
 * the form says so explicitly rather than looking like an empty required field.
 */
export default function CredentialsPage() {
  const [list, setList] = useState<Credential[] | null>(null);
  const [protection, setProtection] = useState("");
  const [registries, setRegistries] = useState<BuiltinRegistry[]>([]);
  const [error, setError] = useState("");
  const [editor, setEditor] = useState<{ open: boolean; credential: Credential | null }>({
    open: false,
    credential: null,
  });
  const [removing, setRemoving] = useState<Credential | null>(null);

  const reload = useCallback(async () => {
    const res = await listCredentials();
    if (res.ok) {
      setList(res.credentials ?? []);
      setProtection(res.protection ?? "");
      setError("");
    } else {
      setError(res.error || "读取凭证失败");
      setList([]);
    }
  }, []);

  useEffect(() => {
    (async () => {
      await reload();
      // Suggestions only: any host can be typed, including a private Harbor.
      const regs = await listRegistries();
      if (regs.ok) setRegistries(regs.registries ?? []);
    })();
  }, [reload]);

  async function remove(c: Credential) {
    const res = await deleteCredential(c.id);
    if (!res.ok) setError(res.error || "删除失败");
    await reload();
  }

  return (
    <div className="grid gap-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold tracking-tight">凭证管理</h1>
          <p className="text-sm text-muted-foreground">
            保存私有仓库的登录信息，之后拉取这些仓库的镜像就不用再带 <code>-u</code> /{" "}
            <code>-p</code>。
          </p>
        </div>
        <Button onClick={() => setEditor({ open: true, credential: null })}>
          <Plus className="mr-1 h-4 w-4" />
          添加凭证
        </Button>
      </div>

      {protection && (
        <Card>
          <CardContent className="flex items-start gap-3 pt-6">
            <KeyRound className="mt-0.5 h-4 w-4 shrink-0 text-muted-foreground" />
            <div className="grid gap-1">
              <span className="text-sm font-medium">加密方式：{protection}</span>
              <span className="text-xs text-muted-foreground">
                密码只以密文保存在本机数据库里，永远不会回传给界面、写进日志或导出到 JSON。
                编辑已有凭证时留空密码即表示「保持原密码不变」。
              </span>
            </div>
          </CardContent>
        </Card>
      )}

      {error && <p className="text-sm text-destructive">{error}</p>}

      <Card>
        <CardHeader>
          <CardTitle>已保存的登录</CardTitle>
          <CardDescription>
            每个仓库地址一条。Docker Hub 的几种写法（docker.io / index.docker.io /
            registry-1.docker.io）会归并成同一条。
          </CardDescription>
        </CardHeader>
        <CardContent className="grid gap-2">
          {list === null && <p className="text-sm text-muted-foreground">加载中…</p>}
          {list !== null && list.length === 0 && (
            <p className="text-sm text-muted-foreground">
              还没有保存任何凭证。公开镜像不需要凭证。
            </p>
          )}
          {(list ?? []).map((c) => (
            <div
              key={c.id}
              className="flex flex-wrap items-center justify-between gap-2 rounded-md border p-3"
            >
              <div className="grid gap-0.5">
                <span className="font-mono text-sm font-medium">{c.host}</span>
                <span className="text-xs text-muted-foreground">
                  用户 {c.username || "—"}　·　{c.kind === "token" ? "访问令牌" : "用户名密码"}
                  {c.note ? `　·　${c.note}` : ""}
                </span>
              </div>
              <div className="flex gap-2">
                <Button
                  variant="outline"
                  size="sm"
                  onClick={() => setEditor({ open: true, credential: c })}
                >
                  <Pencil className="mr-1 h-3.5 w-3.5" />
                  编辑
                </Button>
                <Button variant="outline" size="sm" onClick={() => setRemoving(c)}>
                  <Trash2 className="mr-1 h-3.5 w-3.5" />
                  删除
                </Button>
              </div>
            </div>
          ))}
        </CardContent>
      </Card>

      {/*
        Mounted only while open, and keyed by the row: React gives a fresh
        instance (and therefore fresh initial state) each time, which is what
        clears the password field. Resetting state inside an effect instead
        was both lint-rejected and easy to get wrong.
      */}
      {editor.open && (
        <CredentialEditor
          key={editor.credential?.id ?? "new"}
          credential={editor.credential}
          suggestedHosts={registries.map((r) => r.host)}
          onClose={() => setEditor({ open: false, credential: null })}
          onSaved={async () => {
            setEditor({ open: false, credential: null });
            await reload();
          }}
        />
      )}

      <AlertDialog open={removing !== null} onOpenChange={(open) => !open && setRemoving(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>删除 {removing?.host} 的凭证？</AlertDialogTitle>
            <AlertDialogDescription>
              删除后拉取该仓库的私有镜像需要重新登录。已下载的 tar 文件不受影响。
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>取消</AlertDialogCancel>
            <AlertDialogAction
              onClick={async () => {
                if (removing) await remove(removing);
                setRemoving(null);
              }}
            >
              删除
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </div>
  );
}

function CredentialEditor({
  credential,
  suggestedHosts,
  onClose,
  onSaved,
}: {
  credential: Credential | null;
  suggestedHosts: string[];
  onClose: () => void;
  onSaved: () => void | Promise<void>;
}) {
  const editing = credential !== null;
  // Initial state straight from the row. The parent remounts this component
  // on every open (see the key), so an effect is not needed — and the password
  // field always starts empty, which is the point.
  const [host, setHost] = useState(credential?.host ?? "");
  const [username, setUsername] = useState(credential?.username ?? "");
  const [secret, setSecret] = useState("");
  const [kind, setKind] = useState(credential?.kind ?? "basic");
  const [note, setNote] = useState(credential?.note ?? "");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function save() {
    setError("");
    if (!host.trim()) {
      setError("请填写仓库地址");
      return;
    }
    // Editing with a blank password means "keep it"; creating needs one.
    if (!editing && !secret.trim()) {
      setError("请填写密码或访问令牌");
      return;
    }
    if (kind === "basic" && !username.trim()) {
      setError("用户名密码类型需要填写用户名");
      return;
    }
    setBusy(true);
    try {
      const res = editing
        ? await updateCredential(credential.id, {
            host: host.trim(),
            username: username.trim(),
            kind,
            note: note.trim(),
            secret: secret.trim(),
          })
        : await createCredential({
            host: host.trim(),
            username: username.trim(),
            secret: secret.trim(),
            kind,
            note: note.trim(),
          });
      if (!res.ok) {
        setError(res.error || "保存失败");
        return;
      }
      await onSaved();
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog open onOpenChange={(next) => !next && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{editing ? `编辑 ${credential?.host} 的凭证` : "添加凭证"}</DialogTitle>
          <DialogDescription>
            密码只用于访问该仓库，保存在本机并加密。Docker Hub 官方镜像可写{" "}
            <code>docker.io</code>，其它仓库写完整域名（例如 <code>harbor.internal:5000</code>）。
          </DialogDescription>
        </DialogHeader>

        <div className="grid gap-4">
          <div className="grid gap-2">
            <Label htmlFor="host">仓库地址</Label>
            <Input
              id="host"
              value={host}
              placeholder="ghcr.io"
              onChange={(e) => setHost(e.target.value)}
              list="known-registries"
            />
            <datalist id="known-registries">
              {suggestedHosts.map((h) => (
                <option key={h} value={h} />
              ))}
            </datalist>
          </div>

          <div className="grid gap-2">
            <Label>类型</Label>
            <Select value={kind} onValueChange={(v) => setKind(v ?? "basic")}>
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="basic">用户名 + 密码</SelectItem>
                <SelectItem value="token">访问令牌（不需要用户名）</SelectItem>
              </SelectContent>
            </Select>
          </div>

          {kind === "basic" && (
            <div className="grid gap-2">
              <Label htmlFor="username">用户名</Label>
              <Input
                id="username"
                value={username}
                autoComplete="off"
                onChange={(e) => setUsername(e.target.value)}
              />
            </div>
          )}

          <div className="grid gap-2">
            <Label htmlFor="secret">
              密码 / 访问令牌
              {editing && <span className="text-muted-foreground">（留空表示不修改）</span>}
            </Label>
            <Input
              id="secret"
              type="password"
              value={secret}
              autoComplete="new-password"
              placeholder={editing ? "••••••（保持原密码）" : ""}
              onChange={(e) => setSecret(e.target.value)}
            />
          </div>

          <div className="grid gap-2">
            <Label htmlFor="note">备注（可选）</Label>
            <Input id="note" value={note} onChange={(e) => setNote(e.target.value)} />
          </div>

          {error && <p className="text-sm text-destructive">{error}</p>}
        </div>

        <DialogFooter>
          <Button variant="outline" onClick={onClose} disabled={busy}>
            取消
          </Button>
          <Button onClick={save} disabled={busy}>
            {busy ? "保存中…" : "保存"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
