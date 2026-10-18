import { useRef } from 'react';
import type { KeyboardEvent, ReactElement } from 'react';
import { useTranslation } from 'react-i18next';

// ZoneNav ports renderExpZoneNav (exp_workbench.js ~417): the five
// deep-linkable zone buttons (four zones + the settings & audit tab). The
// active button carries aria-selected; selection routes via onSelect so every
// zone keeps its own hash deep link.
export const ZONE_TABS = ['summary', 'health', 'evidence', 'rollout', 'settings'] as const;
export type ZoneTab = (typeof ZONE_TABS)[number];

const ZONE_LABEL_KEYS: Record<ZoneTab, string> = {
  summary: 'exp.zone.summary',
  health: 'exp.zone.health',
  evidence: 'exp.zone.evidence',
  rollout: 'exp.zone.rollout',
  settings: 'exp.tab.settings',
};

// APG tabs, manual activation: the arrow keys move FOCUS only, Enter/Space
// selects. Zone switches trigger a data fetch plus a hash/history change, so
// automatic activation would fire a navigation (and a history entry) on every
// arrow press — focus movement must stay side-effect free.
export function ZoneNav({
  active,
  onSelect,
}: {
  active: ZoneTab;
  onSelect: (tab: ZoneTab) => void;
}): ReactElement {
  const { t } = useTranslation();
  const navRef = useRef<HTMLElement>(null);

  const onKeyDown = (ev: KeyboardEvent<HTMLElement>) => {
    const tabs = ZONE_TABS;
    // With manual activation the focused tab and the active tab diverge;
    // arrows chain from whichever tab holds focus (fall back to the active
    // tab when focus is somewhere else in the nav).
    const focusedId = document.activeElement?.id || '';
    const focusedTab = focusedId.startsWith('exp-tab-') ? (focusedId.slice('exp-tab-'.length) as ZoneTab) : active;
    const current = tabs.indexOf(focusedTab);
    const base = current >= 0 ? current : tabs.indexOf(active);
    let next = -1;
    if (ev.key === 'ArrowRight') next = (base + 1) % tabs.length;
    else if (ev.key === 'ArrowLeft') next = (base + tabs.length - 1) % tabs.length;
    else if (ev.key === 'Home') next = 0;
    else if (ev.key === 'End') next = tabs.length - 1;
    else return;
    ev.preventDefault();
    navRef.current?.querySelector<HTMLElement>('#exp-tab-' + tabs[next])?.focus();
  };

  return (
    <nav
      className="exp-zone-nav"
      id="exp-zone-nav"
      role="tablist"
      aria-label={t('exp.zones.nav')}
      onKeyDown={onKeyDown}
      ref={navRef}
    >
      {ZONE_TABS.map(name => {
        const isActive = name === active;
        return (
          <button
            key={name}
            type="button"
            role="tab"
            id={'exp-tab-' + name}
            className={'btn exp-zone-tab' + (isActive ? ' active' : '')}
            data-exp-tab={name}
            aria-selected={isActive}
            aria-controls={'exp-zonepanel-' + name}
            tabIndex={isActive ? 0 : -1}
            onClick={() => {
              if (!isActive) onSelect(name);
            }}
          >
            {t(ZONE_LABEL_KEYS[name])}
          </button>
        );
      })}
    </nav>
  );
}
