import host, { HostBridgeCallbacks, HostDisplayInfo, HostMenuItem, HostWinConfig } from 'libmygo_host.so';
import { bundleManager, common } from '@kit.AbilityKit';
import { display, window } from '@kit.ArkUI';
import { i18n } from '@kit.LocalizationKit';
import { hilog } from '@kit.PerformanceAnalysisKit';

const DOMAIN: number = 0x0000;
const TAG: string = 'mygo-bridge';

// Wire flags & constants matching mygo/internal/ohos/proto.go.
const WIN_FLAG_RESIZABLE: number = 1 << 0;
const WIN_FLAG_MINIMIZABLE: number = 1 << 1;
const WIN_FLAG_MAXIMIZABLE: number = 1 << 2;
const WIN_FLAG_CLOSABLE: number = 1 << 3;
const WIN_FLAG_MODAL: number = 1 << 4;
const WIN_FLAG_HIDDEN_TITLEBAR: number = 1 << 5;
const WIN_FLAG_HIDE_WIN_BUTTONS: number = 1 << 6;
const WIN_FLAG_ALWAYS_ON_TOP: number = 1 << 7;
const WIN_FLAG_MAXIMIZED: number = 1 << 8;
const WIN_FLAG_FULLSCREEN: number = 1 << 9;
const WIN_FLAG_CENTER: number = 1 << 10;
const WIN_FLAG_FRAMELESS: number = 1 << 11;
const WIN_FLAG_HAS_BG: number = 1 << 12;

const CFG_MASK_TITLE: number = 1 << 0;
const CFG_MASK_BOUNDS: number = 1 << 1;
const CFG_MASK_MIN_SIZE: number = 1 << 2;
const CFG_MASK_MAX_SIZE: number = 1 << 3;
const CFG_MASK_FLAGS: number = 1 << 4;
const CFG_MASK_BG: number = 1 << 5;
const CFG_MASK_ALL: number = 0x3F;

const WIN_ACT_SHOW: number = 1;
const WIN_ACT_SHOW_INACTIVE: number = 2;
const WIN_ACT_HIDE: number = 3;
const WIN_ACT_CLOSE: number = 4;
const WIN_ACT_FOCUS: number = 5;
const WIN_ACT_BLUR: number = 6;
const WIN_ACT_MINIMIZE: number = 7;
const WIN_ACT_MAXIMIZE: number = 8;
const WIN_ACT_UNMAXIMIZE: number = 9;
const WIN_ACT_RESTORE: number = 10;
const WIN_ACT_ENTER_FULLSCREEN: number = 11;
const WIN_ACT_EXIT_FULLSCREEN: number = 12;
const WIN_ACT_CENTER: number = 13;
const WIN_ACT_START_DRAG: number = 14;
const WIN_ACT_CLOSE_ALLOW: number = 15;
const WIN_ACT_CLOSE_DENY: number = 16;

const WIN_OP_FOCUS: number = 1;
const WIN_OP_BLUR: number = 2;
const WIN_OP_SHOW: number = 3;
const WIN_OP_HIDE: number = 4;
const WIN_OP_MINIMIZE: number = 7;
const WIN_OP_RESTORE: number = 8;
const WIN_OP_MAXIMIZE: number = 9;
const WIN_OP_UNMAXIMIZE: number = 10;
const WIN_OP_ENTER_FULLSCREEN: number = 11;
const WIN_OP_LEAVE_FULLSCREEN: number = 12;
const WIN_OP_CLOSE_REQUEST: number = 13;
const WIN_OP_CLOSED: number = 14;

export type PopupMenuOpener = (reqId: number, x: number, y: number, rtl: boolean, items: HostMenuItem[]) => void;

class ManagedWindow {
  winId: number = 1;
  arkWindowId: number = 0;
  win: window.Window | undefined = undefined;
  ready: boolean = false;
  active: boolean = false;
  closingFromEngine: boolean = false;
  lastStatus: window.WindowStatusType = window.WindowStatusType.FLOATING;
  cfg: HostWinConfig | undefined = undefined;
  pendingActions: number[] = [];
  willCloseResolve: ((prevent: boolean) => void) | undefined = undefined;
}

function rgbaToHexColor(rgba: number): string {
  const r: number = (rgba >>> 24) & 0xFF;
  const g: number = (rgba >>> 16) & 0xFF;
  const b: number = (rgba >>> 8) & 0xFF;
  const a: number = rgba & 0xFF;
  const to2 = (v: number): string => v.toString(16).padStart(2, '0');
  return '#' + to2(a) + to2(r) + to2(g) + to2(b);
}

class HostBridgeManager {
  private context: common.UIAbilityContext | undefined = undefined;
  private windowStage: window.WindowStage | undefined = undefined;
  private windows: Map<number, ManagedWindow> = new Map<number, ManagedWindow>();
  private popupOpeners: Map<number, PopupMenuOpener> = new Map<number, PopupMenuOpener>();
  private displayListenersRegistered: boolean = false;
  private callbacksRegistered: boolean = false;

  init(ctx: common.UIAbilityContext): void {
    this.context = ctx;
    if (!this.callbacksRegistered) {
      this.callbacksRegistered = true;
      const cb: HostBridgeCallbacks = {
        onWinCreate: (cfg: HostWinConfig): void => {
          this.handleWinCreate(cfg);
        },
        onWinConfig: (cfg: HostWinConfig): void => {
          this.handleWinConfig(cfg);
        },
        onWinAction: (winId: number, action: number): void => {
          this.handleWinAction(winId, action);
        },
        onPopupMenu: (reqId: number, winId: number, x: number, y: number, rtl: boolean, items: HostMenuItem[]): void => {
          this.handlePopupMenu(reqId, winId, x, y, rtl, items);
        },
        onEngineQuit: (): void => {
          this.handleEngineQuit();
        },
        onEngineRestart: (): void => {
          this.handleEngineRestart();
        }
      };
      host.setBridgeCallbacks(cb);
    }
    this.refreshEnvAndDisplays();
    this.registerDisplayListeners();
  }

  attachMainStage(stage: window.WindowStage, mainWin: window.Window): void {
    this.windowStage = stage;
    let mw: ManagedWindow | undefined = this.windows.get(1);
    if (mw === undefined) {
      mw = new ManagedWindow();
      mw.winId = 1;
      this.windows.set(1, mw);
    }
    mw.win = mainWin;
    try {
      const props: window.WindowProperties = mainWin.getWindowProperties();
      mw.arkWindowId = props.id;
    } catch (_) {}
    mw.active = true;
    this.attachWindowListeners(mw);
  }

  onMainContentLoaded(): void {
    const mw: ManagedWindow | undefined = this.windows.get(1);
    if (mw === undefined || mw.win === undefined) {
      return;
    }
    mw.ready = true;
    this.reportWindowRect(mw);
    if (mw.cfg !== undefined) {
      this.applyWindowConfig(mw, mw.cfg, CFG_MASK_ALL, true);
    }
    this.flushPendingActions(mw);
  }

  destroyStage(): void {
    this.unregisterDisplayListeners();
    this.windows.forEach((mw: ManagedWindow): void => {
      this.detachWindowListeners(mw);
    });
    this.windows.clear();
    this.popupOpeners.clear();
    this.windowStage = undefined;
  }

  getArkWindowId(winId: number): number {
    const mw: ManagedWindow | undefined = this.windows.get(winId);
    if (mw !== undefined && mw.arkWindowId > 0) {
      return mw.arkWindowId;
    }
    return AppStorage.get<number>('windowId') ?? 0;
  }

  registerPopupMenuOpener(winId: number, opener: PopupMenuOpener): void {
    this.popupOpeners.set(winId, opener);
  }

  unregisterPopupMenuOpener(winId: number): void {
    this.popupOpeners.delete(winId);
  }

  refreshEnvAndDisplays(): void {
    let pkgId: string = 'dev.egoist.music';
    let pkgVer: string = '0.1.0';
    let pkgName: string = 'MyGo Music';
    try {
      const bInfo: bundleManager.BundleInfo = bundleManager.getBundleInfoForSelfSync(
        bundleManager.BundleFlag.GET_BUNDLE_INFO_WITH_APPLICATION
      );
      if (bInfo.name.length > 0) {
        pkgId = bInfo.name;
      }
      if (bInfo.versionName.length > 0) {
        pkgVer = bInfo.versionName;
      }
      if (this.context !== undefined && bInfo.appInfo !== undefined && bInfo.appInfo.labelId > 0) {
        const label: string = this.context.resourceManager.getStringSync(bInfo.appInfo.labelId);
        if (label.length > 0) {
          pkgName = label;
        }
      }
    } catch (_) {}

    let langs: string[] = [];
    let locale: string = '';
    let hourCycle: number = 24;
    let firstWeekday: number = -1;
    try {
      langs = i18n.System.getPreferredLanguageList();
    } catch (_) {}
    try {
      locale = i18n.System.getSystemLocaleInstance().toString();
    } catch (_) {}
    if (langs.length === 0 && locale.length > 0) {
      langs = [locale];
    }
    try {
      hourCycle = i18n.System.is24HourClock() ? 24 : 12;
    } catch (_) {}
    try {
      const wd: number = i18n.System.getFirstDayOfWeek();
      if (wd >= 1 && wd <= 7) {
        firstWeekday = wd % 7;
      }
    } catch (_) {}

    let defaultId: number = 0;
    let rawDisplays: display.Display[] = [];
    try {
      const defDisp: display.Display = display.getDefaultDisplaySync();
      defaultId = defDisp.id;
      rawDisplays = [defDisp];
    } catch (_) {}

    const sendSnapshot = (disps: display.Display[], workAreas: Map<number, display.Rect>): void => {
      const list: HostDisplayInfo[] = [];
      for (let i: number = 0; i < disps.length; i++) {
        const d: display.Display = disps[i];
        const sc: number = d.densityPixels > 0 ? d.densityPixels : 1.0;
        const xDip: number = Math.round((d.x ?? 0) / sc);
        const yDip: number = Math.round((d.y ?? 0) / sc);
        const wDip: number = Math.max(1, Math.round(d.width / sc));
        const hDip: number = Math.max(1, Math.round(d.height / sc));
        let wx: number = xDip;
        let wy: number = yDip;
        let ww: number = wDip;
        let wh: number = hDip;
        const area: display.Rect | undefined = workAreas.get(d.id);
        if (area !== undefined && area.width > 0 && area.height > 0) {
          wx = xDip + Math.round(area.left / sc);
          wy = yDip + Math.round(area.top / sc);
          ww = Math.max(1, Math.round(area.width / sc));
          wh = Math.max(1, Math.round(area.height / sc));
        }
        const label: string = (d.name !== undefined && d.name.length > 0) ? d.name : ('Display ' + d.id.toString());
        const item: HostDisplayInfo = {
          id: d.id,
          label: label,
          x: xDip,
          y: yDip,
          w: wDip,
          h: hDip,
          wx: wx,
          wy: wy,
          ww: ww,
          wh: wh,
          scale: sc,
          rotation: (d.rotation ?? 0) * 90,
          primary: d.id === defaultId,
          internal: d.id === 0
        };
        list.push(item);
      }
      host.updateEnvInfo(pkgName, pkgVer, pkgId, locale, firstWeekday, hourCycle, langs, list);
    };

    const emptyAreas: Map<number, display.Rect> = new Map<number, display.Rect>();
    sendSnapshot(rawDisplays, emptyAreas);

    display.getAllDisplays().then((allDisps: display.Display[]): void => {
      const targetDisps: display.Display[] = allDisps.length > 0 ? allDisps : rawDisplays;
      const areas: Map<number, display.Rect> = new Map<number, display.Rect>();
      let remaining: number = targetDisps.length;
      if (remaining === 0) {
        return;
      }
      for (let i: number = 0; i < targetDisps.length; i++) {
        const cur: display.Display = targetDisps[i];
        cur.getAvailableArea().then((rect: display.Rect): void => {
          areas.set(cur.id, rect);
          remaining--;
          if (remaining === 0) {
            sendSnapshot(targetDisps, areas);
          }
        }).catch((): void => {
          remaining--;
          if (remaining === 0) {
            sendSnapshot(targetDisps, areas);
          }
        });
      }
    }).catch((): void => {});
  }

  private registerDisplayListeners(): void {
    if (this.displayListenersRegistered) {
      return;
    }
    this.displayListenersRegistered = true;
    const onDispEvent = (_id: number): void => {
      this.refreshEnvAndDisplays();
    };
    try {
      display.on('add', onDispEvent);
      display.on('remove', onDispEvent);
      display.on('change', onDispEvent);
    } catch (_) {}
  }

  private unregisterDisplayListeners(): void {
    if (!this.displayListenersRegistered) {
      return;
    }
    this.displayListenersRegistered = false;
    try {
      display.off('add');
      display.off('remove');
      display.off('change');
    } catch (_) {}
  }

  private currentScale(): number {
    try {
      const d: display.Display = display.getDefaultDisplaySync();
      return d.densityPixels > 0 ? d.densityPixels : 1.0;
    } catch (_) {
      return 1.0;
    }
  }

  private handleWinCreate(cfg: HostWinConfig): void {
    if (cfg.winId === 1) {
      let mw: ManagedWindow | undefined = this.windows.get(1);
      if (mw === undefined) {
        mw = new ManagedWindow();
        mw.winId = 1;
        this.windows.set(1, mw);
      }
      mw.cfg = cfg;
      if (mw.ready && mw.win !== undefined) {
        this.applyWindowConfig(mw, cfg, CFG_MASK_ALL, true);
      }
      return;
    }

    if (this.windowStage === undefined) {
      return;
    }
    const mw: ManagedWindow = new ManagedWindow();
    mw.winId = cfg.winId;
    mw.cfg = cfg;
    this.windows.set(cfg.winId, mw);

    const sc: number = this.currentScale();
    const frameless: boolean = (cfg.flags & WIN_FLAG_FRAMELESS) !== 0;
    const modal: boolean = (cfg.flags & WIN_FLAG_MODAL) !== 0;
    const opts: window.SubWindowOptions = {
      title: cfg.title.length > 0 ? cfg.title : 'Window',
      decorEnabled: !frameless,
      isModal: modal
    };
    if (cfg.w > 0 && cfg.h > 0) {
      opts.windowRect = {
        left: Math.round(cfg.x * sc),
        top: Math.round(cfg.y * sc),
        width: Math.max(100, Math.round(cfg.w * sc)),
        height: Math.max(80, Math.round(cfg.h * sc))
      };
    }

    const stage: window.WindowStage = this.windowStage;
    stage.createSubWindowWithOptions('mygo_win_' + cfg.winId.toString(), opts).then((subWin: window.Window): void => {
      if (!this.windows.has(cfg.winId)) {
        subWin.destroyWindow().catch((): void => {});
        return;
      }
      mw.win = subWin;
      try {
        mw.arkWindowId = subWin.getWindowProperties().id;
      } catch (_) {}
      this.attachWindowListeners(mw);

      const storage: LocalStorage = new LocalStorage();
      storage.setOrCreate<number>('winId', cfg.winId);
      storage.setOrCreate<number>('arkWindowId', mw.arkWindowId);

      subWin.loadContent('pages/Index', storage).then((): void => {
        if (!this.windows.has(cfg.winId)) {
          return;
        }
        mw.ready = true;
        if (mw.cfg !== undefined) {
          this.applyWindowConfig(mw, mw.cfg, CFG_MASK_ALL, true);
        }
        this.reportWindowRect(mw);
        this.flushPendingActions(mw);
      }).catch((err: Error): void => {
        hilog.error(DOMAIN, TAG, 'subWindow loadContent failed: %{public}s', err.message);
      });
    }).catch((err: Error): void => {
      hilog.error(DOMAIN, TAG, 'createSubWindowWithOptions failed: %{public}s', err.message);
      this.windows.delete(cfg.winId);
    });
  }

  private mergeConfig(prev: HostWinConfig | undefined, next: HostWinConfig, mask: number): HostWinConfig {
    if (prev === undefined) {
      return next;
    }
    const merged: HostWinConfig = {
      winId: next.winId,
      parentOrMask: prev.parentOrMask,
      x: (mask & CFG_MASK_BOUNDS) !== 0 ? next.x : prev.x,
      y: (mask & CFG_MASK_BOUNDS) !== 0 ? next.y : prev.y,
      w: (mask & CFG_MASK_BOUNDS) !== 0 ? next.w : prev.w,
      h: (mask & CFG_MASK_BOUNDS) !== 0 ? next.h : prev.h,
      minW: (mask & CFG_MASK_MIN_SIZE) !== 0 ? next.minW : prev.minW,
      minH: (mask & CFG_MASK_MIN_SIZE) !== 0 ? next.minH : prev.minH,
      maxW: (mask & CFG_MASK_MAX_SIZE) !== 0 ? next.maxW : prev.maxW,
      maxH: (mask & CFG_MASK_MAX_SIZE) !== 0 ? next.maxH : prev.maxH,
      flags: (mask & CFG_MASK_FLAGS) !== 0 ? next.flags : prev.flags,
      titleBarHeight: next.titleBarHeight > 0 ? next.titleBarHeight : prev.titleBarHeight,
      bgRGBA: (mask & CFG_MASK_BG) !== 0 ? next.bgRGBA : prev.bgRGBA,
      title: (mask & CFG_MASK_TITLE) !== 0 ? next.title : prev.title
    };
    return merged;
  }

  private handleWinConfig(cfg: HostWinConfig): void {
    const mw: ManagedWindow | undefined = this.windows.get(cfg.winId);
    if (mw === undefined) {
      return;
    }
    const mask: number = cfg.parentOrMask;
    mw.cfg = this.mergeConfig(mw.cfg, cfg, mask);
    if (mw.ready && mw.win !== undefined) {
      this.applyWindowConfig(mw, mw.cfg, mask, false);
    }
  }

  private applyWindowConfig(mw: ManagedWindow, cfg: HostWinConfig, mask: number, isCreate: boolean): void {
    const w: window.Window | undefined = mw.win;
    if (w === undefined) {
      return;
    }
    const sc: number = this.currentScale();

    if ((mask & CFG_MASK_TITLE) !== 0 && cfg.title.length > 0) {
      try {
        w.setWindowTitle(cfg.title).catch((): void => {});
      } catch (_) {}
    }

    if ((mask & CFG_MASK_BG) !== 0 && (cfg.flags & WIN_FLAG_HAS_BG) !== 0) {
      try {
        w.setWindowBackgroundColor(rgbaToHexColor(cfg.bgRGBA));
      } catch (_) {}
    }

    if ((mask & (CFG_MASK_MIN_SIZE | CFG_MASK_MAX_SIZE | CFG_MASK_FLAGS)) !== 0) {
      const resizable: boolean = (cfg.flags & WIN_FLAG_RESIZABLE) !== 0;
      const limits: window.WindowLimits = {
        pixelUnit: window.PixelUnit.VP
      };
      if (!resizable && cfg.w > 0 && cfg.h > 0) {
        limits.minWidth = cfg.w;
        limits.maxWidth = cfg.w;
        limits.minHeight = cfg.h;
        limits.maxHeight = cfg.h;
      } else {
        if (cfg.minW > 0) {
          limits.minWidth = cfg.minW;
        }
        if (cfg.minH > 0) {
          limits.minHeight = cfg.minH;
        }
        if (cfg.maxW > 0) {
          limits.maxWidth = cfg.maxW;
        }
        if (cfg.maxH > 0) {
          limits.maxHeight = cfg.maxH;
        }
      }
      if (limits.minWidth !== undefined || limits.minHeight !== undefined ||
          limits.maxWidth !== undefined || limits.maxHeight !== undefined) {
        try {
          w.setWindowLimits(limits).catch((): void => {});
        } catch (_) {}
      }
    }

    if ((mask & CFG_MASK_BOUNDS) !== 0 && (!isCreate || mw.winId !== 1)) {
      if (cfg.w > 0 && cfg.h > 0 && !isCreate) {
        try {
          w.resize(Math.max(100, Math.round(cfg.w * sc)), Math.max(80, Math.round(cfg.h * sc))).catch((): void => {});
        } catch (_) {}
      }
      if ((cfg.flags & WIN_FLAG_CENTER) !== 0) {
        this.centerWindow(mw);
      } else if (!isCreate || cfg.x !== 0 || cfg.y !== 0) {
        try {
          w.moveWindowTo(Math.round(cfg.x * sc), Math.round(cfg.y * sc)).catch((): void => {});
        } catch (_) {}
      }
    } else if (isCreate && mw.winId === 1 && (cfg.flags & WIN_FLAG_CENTER) !== 0) {
      this.centerWindow(mw);
    }

    if ((mask & CFG_MASK_FLAGS) !== 0) {
      const hiddenTB: boolean = (cfg.flags & WIN_FLAG_HIDDEN_TITLEBAR) !== 0;
      const frameless: boolean = (cfg.flags & WIN_FLAG_FRAMELESS) !== 0;
      const hideBtns: boolean = (cfg.flags & WIN_FLAG_HIDE_WIN_BUTTONS) !== 0;
      try {
        w.setWindowDecorVisible(!hiddenTB && !frameless);
        if (hiddenTB && cfg.titleBarHeight > 0) {
          w.setWindowDecorHeight(cfg.titleBarHeight);
        }
      } catch (_) {}

      try {
        if (hideBtns || frameless) {
          w.setWindowTitleButtonVisible(false, false, false);
        } else {
          w.setWindowTitleButtonVisible(
            (cfg.flags & WIN_FLAG_MAXIMIZABLE) !== 0,
            (cfg.flags & WIN_FLAG_MINIMIZABLE) !== 0,
            (cfg.flags & WIN_FLAG_CLOSABLE) !== 0
          );
        }
      } catch (_) {}

      if (hiddenTB && !hideBtns && !frameless) {
        this.reportTitleButtonRect(mw);
      }

      if (mw.winId >= 2 && (cfg.flags & WIN_FLAG_MODAL) === 0) {
        const zLevel: number = (cfg.flags & WIN_FLAG_ALWAYS_ON_TOP) !== 0 ? 100 : 0;
        try {
          w.setSubWindowZLevel(zLevel).catch((): void => {});
        } catch (_) {}
      }

      if (isCreate) {
        if ((cfg.flags & WIN_FLAG_FULLSCREEN) !== 0) {
          try {
            w.maximize(window.MaximizePresentation.ENTER_IMMERSIVE).catch((): void => {});
          } catch (_) {}
        } else if ((cfg.flags & WIN_FLAG_MAXIMIZED) !== 0) {
          try {
            w.maximize(window.MaximizePresentation.EXIT_IMMERSIVE).catch((): void => {});
          } catch (_) {}
        }
      }
    }
  }

  private centerWindow(mw: ManagedWindow): void {
    const w: window.Window | undefined = mw.win;
    if (w === undefined) {
      return;
    }
    try {
      const d: display.Display = display.getDefaultDisplaySync();
      const props: window.WindowProperties = w.getWindowProperties();
      const winW: number = props.windowRect.width > 0 ? props.windowRect.width : Math.round((mw.cfg?.w ?? 960) * this.currentScale());
      const winH: number = props.windowRect.height > 0 ? props.windowRect.height : Math.round((mw.cfg?.h ?? 640) * this.currentScale());
      const targetX: number = Math.max(0, Math.round((d.width - winW) / 2));
      const targetY: number = Math.max(0, Math.round((d.height - winH) / 2));
      w.moveWindowTo(targetX, targetY).catch((): void => {});
    } catch (_) {}
  }

  private handleWinAction(winId: number, action: number): void {
    const mw: ManagedWindow | undefined = this.windows.get(winId);
    if (mw === undefined) {
      return;
    }
    if (action === WIN_ACT_CLOSE_ALLOW || action === WIN_ACT_CLOSE_DENY) {
      const resolve: ((prevent: boolean) => void) | undefined = mw.willCloseResolve;
      mw.willCloseResolve = undefined;
      if (action === WIN_ACT_CLOSE_ALLOW) {
        mw.closingFromEngine = true;
        if (resolve !== undefined) {
          resolve(false);
        }
        if (winId === 1) {
          this.handleEngineQuit();
        } else {
          this.cleanupClosedSubWindow(winId);
        }
      } else {
        if (resolve !== undefined) {
          resolve(true);
        }
      }
      return;
    }

    if (!mw.ready || mw.win === undefined) {
      mw.pendingActions.push(action);
      return;
    }
    this.executeWindowAction(mw, action);
  }

  private flushPendingActions(mw: ManagedWindow): void {
    if (mw.pendingActions.length === 0) {
      return;
    }
    const actions: number[] = mw.pendingActions.slice();
    mw.pendingActions = [];
    for (let i: number = 0; i < actions.length; i++) {
      this.executeWindowAction(mw, actions[i]);
    }
  }

  private executeWindowAction(mw: ManagedWindow, action: number): void {
    const w: window.Window | undefined = mw.win;
    if (w === undefined) {
      return;
    }
    switch (action) {
      case WIN_ACT_SHOW:
      case WIN_ACT_SHOW_INACTIVE:
        try {
          w.showWindow().then((): void => {
            this.reportWindowRect(mw);
            if (action === WIN_ACT_SHOW) {
              this.focusManagedWindow(mw);
            }
          }).catch((): void => {});
        } catch (_) {}
        break;
      case WIN_ACT_HIDE:
        if (mw.winId >= 2) {
          try {
            w.minimize().catch((): void => {});
          } catch (_) {}
        }
        break;
      case WIN_ACT_CLOSE:
        mw.closingFromEngine = true;
        if (mw.winId === 1) {
          this.handleEngineQuit();
        } else {
          this.detachWindowListeners(mw);
          this.windows.delete(mw.winId);
          this.popupOpeners.delete(mw.winId);
          try {
            w.destroyWindow().catch((): void => {});
          } catch (_) {}
        }
        break;
      case WIN_ACT_FOCUS:
        this.focusManagedWindow(mw);
        break;
      case WIN_ACT_BLUR:
        if (mw.winId >= 2) {
          const mainMw: ManagedWindow | undefined = this.windows.get(1);
          if (mainMw !== undefined) {
            this.focusManagedWindow(mainMw);
          }
        }
        break;
      case WIN_ACT_MINIMIZE:
        try {
          w.minimize().catch((): void => {});
        } catch (_) {}
        break;
      case WIN_ACT_MAXIMIZE:
        try {
          w.maximize(window.MaximizePresentation.EXIT_IMMERSIVE).catch((): void => {});
        } catch (_) {}
        break;
      case WIN_ACT_UNMAXIMIZE:
      case WIN_ACT_RESTORE:
      case WIN_ACT_EXIT_FULLSCREEN:
        try {
          w.recover().catch((): void => {});
        } catch (_) {}
        break;
      case WIN_ACT_ENTER_FULLSCREEN:
        try {
          w.maximize(window.MaximizePresentation.ENTER_IMMERSIVE).catch((): void => {});
        } catch (_) {}
        break;
      case WIN_ACT_CENTER:
        this.centerWindow(mw);
        break;
      case WIN_ACT_START_DRAG:
        try {
          w.startMoving().catch((): void => {});
        } catch (_) {}
        break;
      default:
        break;
    }
  }

  private focusManagedWindow(target: ManagedWindow): void {
    if (target.arkWindowId <= 0) {
      return;
    }
    let activeId: number = 0;
    this.windows.forEach((cand: ManagedWindow): void => {
      if (cand.active && cand.arkWindowId > 0) {
        activeId = cand.arkWindowId;
      }
    });
    if (activeId > 0 && activeId !== target.arkWindowId) {
      try {
        window.shiftAppWindowFocus(activeId, target.arkWindowId).catch((): void => {});
      } catch (_) {}
    }
  }

  private attachWindowListeners(mw: ManagedWindow): void {
    const w: window.Window | undefined = mw.win;
    if (w === undefined) {
      return;
    }
    const winId: number = mw.winId;

    try {
      w.on('windowEvent', (evType: window.WindowEventType): void => {
        switch (evType) {
          case window.WindowEventType.WINDOW_SHOWN:
            host.notifyWindowEvent(winId, WIN_OP_SHOW);
            break;
          case window.WindowEventType.WINDOW_ACTIVE:
            mw.active = true;
            host.notifyWindowEvent(winId, WIN_OP_FOCUS);
            break;
          case window.WindowEventType.WINDOW_INACTIVE:
            mw.active = false;
            host.notifyWindowEvent(winId, WIN_OP_BLUR);
            break;
          case window.WindowEventType.WINDOW_HIDDEN:
            mw.active = false;
            host.notifyWindowEvent(winId, WIN_OP_HIDE);
            break;
          default:
            break;
        }
      });
    } catch (_) {}

    try {
      w.on('windowStatusChange', (status: window.WindowStatusType): void => {
        const prev: window.WindowStatusType = mw.lastStatus;
        mw.lastStatus = status;
        if (status === window.WindowStatusType.MINIMIZE) {
          host.notifyWindowEvent(winId, WIN_OP_MINIMIZE);
        } else if (status === window.WindowStatusType.MAXIMIZE) {
          host.notifyWindowEvent(winId, WIN_OP_MAXIMIZE);
        } else if (status === window.WindowStatusType.FULL_SCREEN) {
          host.notifyWindowEvent(winId, WIN_OP_ENTER_FULLSCREEN);
        } else if (status === window.WindowStatusType.FLOATING) {
          if (prev === window.WindowStatusType.MINIMIZE) {
            host.notifyWindowEvent(winId, WIN_OP_RESTORE);
          } else if (prev === window.WindowStatusType.FULL_SCREEN) {
            host.notifyWindowEvent(winId, WIN_OP_LEAVE_FULLSCREEN);
          } else if (prev === window.WindowStatusType.MAXIMIZE) {
            host.notifyWindowEvent(winId, WIN_OP_UNMAXIMIZE);
          }
        }
      });
    } catch (_) {}

    try {
      w.on('windowRectChange', (opts: window.RectChangeOptions): void => {
        const sc: number = this.currentScale();
        const xDip: number = Math.round(opts.rect.left / sc);
        const yDip: number = Math.round(opts.rect.top / sc);
        const wDip: number = Math.round(opts.rect.width / sc);
        const hDip: number = Math.round(opts.rect.height / sc);
        host.notifyWindowRect(winId, xDip, yDip, wDip, hDip);
      });
    } catch (_) {}

    try {
      w.on('windowTitleButtonRectChange', (rect: window.TitleButtonRect): void => {
        const hDip: number = Math.max(0, Math.round(rect.top + rect.height));
        const rDip: number = Math.max(0, Math.round(rect.right + rect.width));
        host.notifyTitleBar(winId, hDip, 0, rDip);
      });
    } catch (_) {}

    try {
      w.on('windowWillClose', (): Promise<boolean> => {
        if (mw.closingFromEngine) {
          return Promise.resolve(false);
        }
        return new Promise<boolean>((resolve: (prevent: boolean) => void): void => {
          mw.willCloseResolve = resolve;
          host.notifyWindowEvent(winId, WIN_OP_CLOSE_REQUEST);
        });
      });
    } catch (_) {}
  }

  private detachWindowListeners(mw: ManagedWindow): void {
    const w: window.Window | undefined = mw.win;
    if (w === undefined) {
      return;
    }
    try { w.off('windowEvent'); } catch (_) {}
    try { w.off('windowStatusChange'); } catch (_) {}
    try { w.off('windowRectChange'); } catch (_) {}
    try { w.off('windowTitleButtonRectChange'); } catch (_) {}
    try { w.off('windowWillClose'); } catch (_) {}
  }

  private cleanupClosedSubWindow(winId: number): void {
    const mw: ManagedWindow | undefined = this.windows.get(winId);
    if (mw === undefined) {
      return;
    }
    this.detachWindowListeners(mw);
    this.windows.delete(winId);
    this.popupOpeners.delete(winId);
    if (mw.win !== undefined) {
      try {
        mw.win.destroyWindow().catch((): void => {});
      } catch (_) {}
    }
  }

  private reportWindowRect(mw: ManagedWindow): void {
    if (mw.win === undefined) {
      return;
    }
    try {
      const r: window.Rect = mw.win.getWindowProperties().windowRect;
      const sc: number = this.currentScale();
      host.notifyWindowRect(
        mw.winId,
        Math.round(r.left / sc),
        Math.round(r.top / sc),
        Math.round(r.width / sc),
        Math.round(r.height / sc)
      );
    } catch (_) {}
  }

  private reportTitleButtonRect(mw: ManagedWindow): void {
    if (mw.win === undefined) {
      return;
    }
    try {
      const rect: window.TitleButtonRect = mw.win.getTitleButtonRect();
      if (rect.width > 0 && rect.height > 0) {
        const hDip: number = Math.max(0, Math.round(rect.top + rect.height));
        const rDip: number = Math.max(0, Math.round(rect.right + rect.width));
        host.notifyTitleBar(mw.winId, hDip, 0, rDip);
      }
    } catch (_) {}
  }

  private handlePopupMenu(reqId: number, winId: number, x: number, y: number, rtl: boolean, items: HostMenuItem[]): void {
    const opener: PopupMenuOpener | undefined = this.popupOpeners.get(winId) ?? this.popupOpeners.get(1);
    if (opener === undefined) {
      host.resolvePopupMenu(reqId, 0);
      return;
    }
    opener(reqId, x, y, rtl, items);
  }

  private handleEngineQuit(): void {
    if (this.context !== undefined) {
      try {
        this.context.terminateSelf().catch((): void => {});
      } catch (_) {}
    }
  }

  private handleEngineRestart(): void {
    const staleIds: number[] = [];
    this.windows.forEach((_mw: ManagedWindow, id: number): void => {
      if (id >= 2) {
        staleIds.push(id);
      }
    });
    for (let i: number = 0; i < staleIds.length; i++) {
      this.cleanupClosedSubWindow(staleIds[i]);
    }
  }
}

export const hostBridge: HostBridgeManager = new HostBridgeManager();
