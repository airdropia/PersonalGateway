<script>
  import Sidebar from "$lib/components/organisms/Sidebar.svelte";
  import AuthDialog from "$lib/components/organisms/AuthDialog.svelte";
  import TypedConfirmationDialog from "$lib/components/organisms/TypedConfirmationDialog.svelte";
  import FlashMessages from "$lib/components/organisms/FlashMessages.svelte";
  import DemoModeBanner from "$lib/components/molecules/DemoModeBanner.svelte";
  import { router } from "$lib/stores/router.svelte.js";
  import { auth } from "$lib/stores/auth.svelte.js";
  import { themeStore, sidebar, modals } from "$lib/stores/ui.svelte.js";
  import { timezone } from "$lib/stores/timezone.svelte.js";
  import { dateRange } from "$lib/stores/dateRange.svelte.js";
  import { runtimeConfig } from "$lib/stores/runtimeConfig.svelte.js";
  import { modelsStore } from "$lib/stores/models.svelte.js";
  import { versionStore } from "$lib/stores/version.svelte.js";
  import { syncDocumentLocale } from "$lib/i18n/locale.js";

  import OverviewPage from "$pages/overview/OverviewPage.svelte";
  import UsagePage from "$pages/usage/UsagePage.svelte";
  import ModelsPage from "$pages/models/ModelsPage.svelte";
  import PlaygroundPage from "$pages/playground/PlaygroundPage.svelte";
  import ProvidersConfigPage from "$pages/providers-config/ProvidersConfigPage.svelte";
  import AuthKeysPage from "$pages/auth-keys/AuthKeysPage.svelte";
  import SettingsPage from "$pages/settings/SettingsPage.svelte";

  const pageComponents = {
    overview: OverviewPage,
    usage: UsagePage,
    models: ModelsPage,
    playground: PlaygroundPage,
    "providers-config": ProvidersConfigPage,
    "auth-keys": AuthKeysPage,
    settings: SettingsPage,
  };

  syncDocumentLocale();
  timezone.init();
  dateRange.init(); // after timezone.init(): "today" is timezone dependent
  auth.init();
  themeStore.init();
  sidebar.init();
  router.init();
  versionStore.init();

  // Shared inventory refetch on boot and whenever the API key changes.
  $effect(() => {
    void auth.refreshTick;
    runtimeConfig.fetch();
    modelsStore.fetchModels();
    modelsStore.fetchCategories();
  });

  // Body-level modal class (scroll lock while any overlay dialog is open).
  $effect(() => {
    document.body.classList.toggle("dashboard-modal-open", modals.anyOpen);
  });

  const PageComponent = $derived(pageComponents[router.page] || OverviewPage);
</script>

<Sidebar />
<main id="dashboard-content" class="content">
  <DemoModeBanner />
  <PageComponent />
</main>
<AuthDialog />
<TypedConfirmationDialog />
<FlashMessages />